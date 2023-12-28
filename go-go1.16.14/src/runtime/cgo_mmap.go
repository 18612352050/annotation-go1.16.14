// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Support for memory sanitizer. See runtime/cgo/mmap.go.

// +build linux,amd64 linux,arm64

package runtime

import "unsafe"

// _cgo_mmap is filled in by runtime/cgo when it is linked into the
// program, so it is only non-nil when using cgo.
//go:linkname _cgo_mmap _cgo_mmap
var _cgo_mmap unsafe.Pointer

// _cgo_munmap is filled in by runtime/cgo when it is linked into the
// program, so it is only non-nil when using cgo.
//go:linkname _cgo_munmap _cgo_munmap
var _cgo_munmap unsafe.Pointer

// mmap is used to route the mmap system call through C code when using cgo,
// to support sanitizer interceptors. Don't allow stack splits,
// since this function (used by sysAlloc) is called in a lot of low-level parts of the runtime and callers often assume it won't acquire any locks.
// 注释：mmap（内存映射）： mmap 是一个系统调用，用于将文件或设备映射到内存中。它是在使用 cgo（Go语言的外部C语言调用接口）时通过C代码来处理 mmap 系统调用的一种方式
// 注释：cgo： cgo 是Go语言的一个工具，允许在Go程序中调用C语言的函数。在这里，它用于与C代码一起处理 mmap 系统调用，以支持卫生间（sanitizer）拦截器。
// 注释：sanitizer interceptors： 卫生间拦截器是一种用于检测和报告程序中潜在问题（如内存泄漏或越界访问）的工具。在这里，mmap 被用于支持卫生间拦截器，可能是通过将内存映射到特定的区域以进行监控。
//go:nosplit
func mmap(addr unsafe.Pointer, n uintptr, prot, flags, fd int32, off uint32) (unsafe.Pointer, int) {
	if _cgo_mmap != nil {
		// Make ret a uintptr so that writing to it in the
		// function literal does not trigger a write barrier.
		// A write barrier here could break because of the way
		// that mmap uses the same value both as a pointer and
		// an errno value.
		var ret uintptr
		systemstack(func() {
			ret = callCgoMmap(addr, n, prot, flags, fd, off)
		})
		if ret < 4096 {
			return nil, int(ret)
		}
		return unsafe.Pointer(ret), 0
	}
	return sysMmap(addr, n, prot, flags, fd, off)
}

func munmap(addr unsafe.Pointer, n uintptr) {
	if _cgo_munmap != nil {
		systemstack(func() { callCgoMunmap(addr, n) })
		return
	}
	sysMunmap(addr, n)
}

// sysMmap calls the mmap system call. It is implemented in assembly.
func sysMmap(addr unsafe.Pointer, n uintptr, prot, flags, fd int32, off uint32) (p unsafe.Pointer, err int)

// callCgoMmap calls the mmap function in the runtime/cgo package
// using the GCC calling convention. It is implemented in assembly.
func callCgoMmap(addr unsafe.Pointer, n uintptr, prot, flags, fd int32, off uint32) uintptr

// sysMunmap calls the munmap system call. It is implemented in assembly.
func sysMunmap(addr unsafe.Pointer, n uintptr)

// callCgoMunmap calls the munmap function in the runtime/cgo package
// using the GCC calling convention. It is implemented in assembly.
func callCgoMunmap(addr unsafe.Pointer, n uintptr)
