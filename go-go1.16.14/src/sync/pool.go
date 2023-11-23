// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"runtime"
	"sync/atomic"
	"unsafe"
)

// A Pool is a set of temporary objects that may be individually saved and
// retrieved.
//
// Any item stored in the Pool may be removed automatically at any time without
// notification. If the Pool holds the only reference when this happens, the
// item might be deallocated.
//
// A Pool is safe for use by multiple goroutines simultaneously.
//
// Pool's purpose is to cache allocated but unused items for later reuse,
// relieving pressure on the garbage collector. That is, it makes it easy to
// build efficient, thread-safe free lists. However, it is not suitable for all
// free lists.
//
// An appropriate use of a Pool is to manage a group of temporary items
// silently shared among and potentially reused by concurrent independent
// clients of a package. Pool provides a way to amortize allocation overhead
// across many clients.
//
// An example of good use of a Pool is in the fmt package, which maintains a
// dynamically-sized store of temporary output buffers. The store scales under
// load (when many goroutines are actively printing) and shrinks when
// quiescent.
//
// On the other hand, a free list maintained as part of a short-lived object is
// not a suitable use for a Pool, since the overhead does not amortize well in
// that scenario. It is more efficient to have such objects implement their own
// free list.
//
// A Pool must not be copied after first use.
type Pool struct {
	// 用于检测 Pool 池是否被 copy，因为 Pool 不希望被 copy。用这个字段可以在 go vet 工具中检测出被 copy（在编译期间就发现问题）
	noCopy noCopy

	//实际指向 [P]poolLocal，数组大小等于 P 的数量；每个 P 一一对应一个 poolLocal
	local unsafe.Pointer // local fixed-size per-P pool, actual type is [P]poolLocal
	//[P]poolLocal 的大小,因为 P 可以在运行时通过调用 runtime.GOMAXPROCS 进行修改, 因此我们还是得通过 localSize 来对应 local 数组的长度
	localSize uintptr // size of the local array

	// victim 和 victimSize 在 (GC)poolCleanup 流程里赋值为 local 和 localSize。victim 机制是把 Pool 池的清理由一轮 GC 改成两轮 GC，进而提高对象的复用率，减少抖动
	victim     unsafe.Pointer // local from previous cycle
	victimSize uintptr        // size of victims array

	// New optionally specifies a function to generate
	// a value when Get would otherwise return nil.
	// It may not be changed concurrently with calls to Get.
	//函数变量 New 对外暴露，使用者定义对象初始化构造行为
	New func() interface{}
}

// Local per-P Pool appendix.
type poolLocalInternal struct {
	//Get 和 Put 操作都会优先存取 private 变量
	private interface{} // Can be used only by the respective P.
	//本地对象池
	shared poolChain // Local P can pushHead/popHead; any P can popTail.
}

type poolLocal struct {
	poolLocalInternal

	// Prevents false sharing on widespread platforms with
	// 128 mod (cache line size) = 0 .
	//为了防止P 同时在两个不同的 CPU 核上运行，填补128，如果运行在两个不同的cpu，将会同时去覆盖刷新 CacheLine，造成 Cacheline 的反复失效
	pad [128 - unsafe.Sizeof(poolLocalInternal{})%128]byte
}

// from runtime
func fastrand() uint32

var poolRaceHash [128]uint64

// poolRaceAddr returns an address to use as the synchronization point for race detector logic.
// We don't use the actual pointer stored in x directly,
// for fear of conflicting with other synchronization on that address.
// Instead, we hash the pointer to get an index into poolRaceHash.
// See discussion on golang.org/cl/31589.
func poolRaceAddr(x interface{}) unsafe.Pointer {
	ptr := uintptr((*[2]unsafe.Pointer)(unsafe.Pointer(&x))[1])
	h := uint32((uint64(uint32(ptr)) * 0x85ebca6b) >> 16)
	return unsafe.Pointer(&poolRaceHash[h%uint32(len(poolRaceHash))])
}

// Put adds x to the pool.
func (p *Pool) Put(x interface{}) {
	if x == nil {
		return
	}
	if race.Enabled {
		if fastrand()%4 == 0 {
			// Randomly drop x on floor.
			return
		}
		race.ReleaseMerge(poolRaceAddr(x))
		race.Disable()
	}
	//加Pin，锁住当前的G和M
	l, _ := p.pin()
	if l.private == nil {
		//private不存在，将缓存对象放入private中
		l.private = x
		x = nil
	}
	if x != nil {
		//private中已经存在数据，将缓存对象放入shared 双向链表中
		l.shared.pushHead(x)
	}
	// G-M 锁定解除，pin中有加锁
	runtime_procUnpin()
	if race.Enabled {
		race.Enable()
	}
}

// Get selects an arbitrary item from the Pool, removes it from the
// Pool, and returns it to the caller.
// Get may choose to ignore the pool and treat it as empty.
// Callers should not assume any relation between values passed to Put and
// the values returned by Get.
//
// If Get would otherwise return nil and p.New is non-nil, Get returns
// the result of calling p.New.
func (p *Pool) Get() interface{} {
	if race.Enabled {
		race.Disable()
	}
	l, pid := p.pin()
	//Get先从本地local的private中获取元素，此private只能被当前的P访问,一个P同时只能运行一个G， 所以直接访问l.privated不需要加锁
	x := l.private
	l.private = nil
	if x == nil {
		// Try to pop the head of the local shard. We prefer
		// the head over the tail for temporal locality of
		// reuse.
		//如果local.private中没有元素了，尝试从local.shared队列的头部弹出一个元素。
		x, _ = l.shared.popHead()
		if x == nil {
			//如果local.shared中也没有元素了，则从其他P对应的shared中取一个
			//如果其他P对应的shared中也没有元素了，再检查victim中是否有元素
			x = p.getSlow(pid)
		}
	}
	runtime_procUnpin()
	if race.Enabled {
		race.Enable()
		if x != nil {
			race.Acquire(poolRaceAddr(x))
		}
	}
	if x == nil && p.New != nil {
		//如果还是没有，设置了New方法，将调用New方法产生一个
		x = p.New()
	}
	return x
}

func (p *Pool) getSlow(pid int) interface{} {
	// See the comment in pin regarding ordering of the loads.
	size := runtime_LoadAcquintptr(&p.localSize) // load-acquire
	locals := p.local                            // load-consume
	// Try to steal one element from other procs.
	// 遍历locals列表，从其他的local的shared列表尾部获取对象
	for i := 0; i < int(size); i++ {
		l := indexLocal(locals, (pid+i+1)%int(size))
		if x, _ := l.shared.popTail(); x != nil {
			return x
		}
	}

	// Try the victim cache. We do this after attempting to steal
	// from all primary caches because we want objects in the
	// victim cache to age out if at all possible.
	//victim 是上一轮被清理的对象池
	size = atomic.LoadUintptr(&p.victimSize)
	if uintptr(pid) >= size {
		return nil
	}
	locals = p.victim
	l := indexLocal(locals, pid)
	// victim的private不为空则返回
	if x := l.private; x != nil {
		l.private = nil
		return x
	}
	//  遍历victim对应的locals列表，从其他的local的shared列表尾部获取对象
	for i := 0; i < int(size); i++ {
		l := indexLocal(locals, (pid+i)%int(size))
		if x, _ := l.shared.popTail(); x != nil {
			return x
		}
	}

	// Mark the victim cache as empty for future gets don't bother
	// with it.
	// 获取不到，将victimSize置为0,防止下一次从victim中查找数据
	atomic.StoreUintptr(&p.victimSize, 0)

	return nil
}

// pin pins the current goroutine to P, disables preemption and returns poolLocal pool for the P and the P's id.
// Caller must call runtime_procUnpin() when done with the pool.
func (p *Pool) pin() (*poolLocal, int) {
	//会先获取当前G，然后绑定到对应的M上，然后返回M目前绑定的P的id
	pid := runtime_procPin()
	// In pinSlow we store to local and then to localSize, here we load in opposite order.
	// Since we've disabled preemption, GC cannot happen in between.
	// Thus here we must observe local at least as large localSize.
	// We can observe a newer/larger local, it is fine (we must observe its zero-initialized-ness).
	//原子操作取出localSize
	s := runtime_LoadAcquintptr(&p.localSize) // load-acquire
	l := p.local                              // load-consume
	//如果pid小于localSize，调用indexLocal取出pid对应的poolLocal返回
	if uintptr(pid) < s {
		return indexLocal(l, pid), pid
	}
	//Pool还没创建对应的poolLocal，调用pinSlow进行创建
	return p.pinSlow()
}

func (p *Pool) pinSlow() (*poolLocal, int) {
	// Retry under the mutex.
	// Can not lock the mutex while pinned.
	//解除Pin
	runtime_procUnpin()
	//加allPoolsMu一个全局Mutex锁
	allPoolsMu.Lock()
	defer allPoolsMu.Unlock()
	//加Pin
	pid := runtime_procPin()
	// poolCleanup won't be called while we are pinned.
	s := p.localSize
	l := p.local
	//重新检查pid和localSize
	if uintptr(pid) < s {
		return indexLocal(l, pid), pid
	}
	if p.local == nil {
		//初始化local前会将pool放入到allPools数组中
		allPools = append(allPools, p)
	}
	// If GOMAXPROCS changes between GCs, we re-allocate the array and lose the old one.
	//初始化local
	size := runtime.GOMAXPROCS(0)
	local := make([]poolLocal, size)
	atomic.StorePointer(&p.local, unsafe.Pointer(&local[0])) // store-release
	runtime_StoreReluintptr(&p.localSize, uintptr(size))     // store-release
	return &local[pid], pid
}

func poolCleanup() {
	// This function is called with the world stopped, at the beginning of a garbage collection.
	// It must not allocate and probably should not call any runtime functions.

	// Because the world is stopped, no pool user can be in a
	// pinned section (in effect, this has all Ps pinned).

	// Drop victim caches from all pools.
	for _, p := range oldPools {
		//清空oldPools中 victim 的对象
		p.victim = nil
		p.victimSize = 0
	}

	// Move primary cache to victim cache.
	for _, p := range allPools {
		//将allPools对象池中，local对象迁移到 victim上
		p.victim = p.local
		p.victimSize = p.localSize
		p.local = nil
		p.localSize = 0
	}

	// The pools with non-empty primary caches now have non-empty
	// victim caches and no pools have primary caches.
	//将allPools迁移到oldPools，并清空allPools
	oldPools, allPools = allPools, nil
}

var (
	allPoolsMu Mutex

	// allPools is the set of pools that have non-empty primary
	// caches. Protected by either 1) allPoolsMu and pinning or 2)
	// STW.
	allPools []*Pool

	// oldPools is the set of pools that may have non-empty victim
	// caches. Protected by STW.
	oldPools []*Pool
)

func init() {
	runtime_registerPoolCleanup(poolCleanup)
}

func indexLocal(l unsafe.Pointer, i int) *poolLocal {
	lp := unsafe.Pointer(uintptr(l) + uintptr(i)*unsafe.Sizeof(poolLocal{}))
	return (*poolLocal)(lp)
}

// Implemented in runtime.
func runtime_registerPoolCleanup(cleanup func())
func runtime_procPin() int
func runtime_procUnpin()

// The below are implemented in runtime/internal/atomic and the
// compiler also knows to intrinsify the symbol we linkname into this
// package.

//go:linkname runtime_LoadAcquintptr runtime/internal/atomic.LoadAcquintptr
func runtime_LoadAcquintptr(ptr *uintptr) uintptr

//go:linkname runtime_StoreReluintptr runtime/internal/atomic.StoreReluintptr
func runtime_StoreReluintptr(ptr *uintptr, val uintptr) uintptr
