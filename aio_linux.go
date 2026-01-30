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

//go:build linux

package gaio

import (
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// _EPOLLET value is incorrect in syscall.
const (
	_EPOLLET      = 0x80000000
	_EFD_NONBLOCK = 0x800
)

// poller is an epoll-based poller.
type poller struct {
	cpuid  int32      // the CPU ID to bind to
	mu     sync.Mutex // mutex to protect fd closure
	pfd    int        // epoll fd
	efd    int        // eventfd
	efdbuf []byte

	// shutdown signal
	die     chan struct{}
	dieOnce sync.Once
}

func openPoll() (*poller, error) {
	fd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}
	r0, _, e0 := syscall.Syscall(syscall.SYS_EVENTFD2, 0, _EFD_NONBLOCK, 0)
	if e0 != 0 {
		syscall.Close(fd)
		return nil, e0 // Fix: return actual syscall error instead of previous nil error
	}

	if err := syscall.EpollCtl(fd, syscall.EPOLL_CTL_ADD, int(r0),
		&syscall.EpollEvent{Fd: int32(r0),
			Events: syscall.EPOLLIN | _EPOLLET,
		},
	); err != nil {
		syscall.Close(fd)
		syscall.Close(int(r0))
		return nil, err
	}

	p := new(poller)
	p.pfd = fd
	p.efd = int(r0)
	p.efdbuf = make([]byte, 8)
	p.die = make(chan struct{})
	p.cpuid = -1

	return p, err
}

// Close shuts down the poller.
func (p *poller) Close() error {
	p.dieOnce.Do(func() {
		close(p.die)
	})
	return p.wakeup()
}

func (p *poller) Watch(fd int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return syscall.EpollCtl(p.pfd, syscall.EPOLL_CTL_ADD, fd, &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLRDHUP | syscall.EPOLLIN | syscall.EPOLLOUT | _EPOLLET})
}

// wakeup interrupts epoll_wait.
func (p *poller) wakeup() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.efd == -1 {
		return ErrPollerClosed
	}
	var x uint64 = 1
	// eventfd is set with EFD_NONBLOCK
	_, err := syscall.Write(p.efd, (*(*[8]byte)(unsafe.Pointer(&x)))[:])
	return err
}

func (p *poller) Wait(chSignal chan Signal) {
	// Pre-allocate event set with typical capacity to reduce allocations
	eventSet := make(pollerEvents, 0, maxEvents)
	events := make([]syscall.EpollEvent, maxEvents)
	sig := Signal{
		done: make(chan struct{}, 1),
	}

	// close poller fd & eventfd in defer
	defer func() {
		p.mu.Lock()
		syscall.Close(p.pfd)
		syscall.Close(p.efd)
		p.pfd = -1
		p.efd = -1
		p.mu.Unlock()
	}()

	const (
		rSet = syscall.EPOLLIN | syscall.EPOLLRDHUP
		wSet = syscall.EPOLLOUT
		eSet = syscall.EPOLLERR | syscall.EPOLLHUP
	)

	// epoll event loop
	for {
		select {
		case <-p.die:
			return
		default:
			n, err := syscall.EpollWait(p.pfd, events, -1)
			if err == syscall.EINTR {
				continue
			}
			if err != nil {
				return
			}

			// Event processing - use an index-based loop to avoid allocation.
			for i := 0; i < n; i++ {
				ev := &events[i]
				if int(ev.Fd) == p.efd {
					syscall.Read(p.efd, p.efdbuf) // simply consume
					if cpuid := atomic.LoadInt32(&p.cpuid); cpuid != -1 {
						setAffinity(cpuid)
						atomic.StoreInt32(&p.cpuid, -1)
					}
				} else {
					e := event{ident: int(ev.Fd)}

					// EPOLLERR/EPOLLHUP should wake both read and write waiters.
					if ev.Events&eSet != 0 {
						e.ev |= EV_READ | EV_WRITE
					}

					// EPOLLRDHUP (since Linux 2.6.17)
					// Stream socket peer closed connection, or shut down writing
					// half of connection.  (This flag is especially useful for writ-
					// ing simple code to detect peer shutdown when using Edge Trig-
					// gered monitoring.)
					if ev.Events&rSet != 0 {
						e.ev |= EV_READ
					}
					if ev.Events&wSet != 0 {
						e.ev |= EV_WRITE
					}

					eventSet = append(eventSet, e)
				}
			}

			// notify watcher
			sig.events = eventSet

			select {
			case chSignal <- sig:
			case <-p.die:
				return
			}

			// wait for the watcher to finish processing
			select {
			case <-sig.done:
				eventSet = eventSet[:0:cap(eventSet)]
			case <-p.die:
				return
			}
		}
	}
}

// rawRead performs non-blocking reads to avoid context switches.
// NOTE: r0 is set to -1 on error, which becomes MaxUint when converted to int on 64-bit
func rawRead(fd int, p []byte) (n int, err error) {
	var _p0 unsafe.Pointer
	if len(p) > 0 {
		_p0 = unsafe.Pointer(&p[0])
	} else {
		_p0 = unsafe.Pointer(&_zero)
	}
	r0, _, e1 := syscall.RawSyscall(syscall.SYS_READ, uintptr(fd), uintptr(_p0), uintptr(len(p)))
	if e1 != 0 {
		return -1, e1
	}
	return int(r0), nil
}

// rawWrite performs non-blocking writes to avoid context switches.
// NOTE: r0 is set to -1 on error, which becomes MaxUint when converted to int on 64-bit
func rawWrite(fd int, p []byte) (n int, err error) {
	var _p0 unsafe.Pointer
	if len(p) > 0 {
		_p0 = unsafe.Pointer(&p[0])
	} else {
		_p0 = unsafe.Pointer(&_zero)
	}
	r0, _, e1 := syscall.RawSyscall(syscall.SYS_WRITE, uintptr(fd), uintptr(_p0), uintptr(len(p)))
	if e1 != 0 {
		return -1, e1
	}
	return int(r0), nil
}
