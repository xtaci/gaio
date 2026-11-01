// The MIT License (MIT)
//
// Copyright (c) 2019 xtaci
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

//go:build windows

package gaio

import (
	"runtime"
	"syscall"
	"unsafe"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procSetThreadAffinityMask = kernel32.NewProc("SetThreadAffinityMask")
	procGetCurrentThread      = kernel32.NewProc("GetCurrentThread")
)

// setAffinity pins the current goroutine to its OS thread and asks Windows to
// restrict that thread to the specified CPU.
func setAffinity(cpuId int32) {
	runtime.LockOSThread()

	// Get the pseudo-handle for the current thread.
	thread, _, _ := procGetCurrentThread.Call()

	// Set only the bit corresponding to the target CPU.
	var mask uintptr = 1 << uint(cpuId)
	procSetThreadAffinityMask.Call(thread, uintptr(unsafe.Pointer(&mask)))
}
