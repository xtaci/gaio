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

//go:build darwin || netbsd || freebsd || openbsd || dragonfly

package gaio

import (
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// poller represents a kqueue-based poller for monitoring file descriptors.
type poller struct {
	cpuid int32
	mu    sync.Mutex // mutex to protect fd closing
	fd    int        // kqueue fd

	// watchQueue for poll
	watchQueue      []int
	watchQueueMutex sync.Mutex
	// closing signal
	die     chan struct{}
	dieOnce sync.Once
}

// openPoll creates and initializes a new poller.
func openPoll() (*poller, error) {
	fd, err := syscall.Kqueue()
	if err != nil {
		return nil, err
	}

	_, err = syscall.Kevent(fd, []syscall.Kevent_t{{
		Ident:  0,
		Filter: syscall.EVFILT_USER,
		Flags:  syscall.EV_ADD | syscall.EV_CLEAR,
	}}, nil, nil)

	if err != nil {
		return nil, err
	}

	p := new(poller)
	p.fd = fd
	p.die = make(chan struct{})
	p.cpuid = -1

	return p, nil
}

// Close shuts down the poller, ensuring cleanup is done only once.
func (p *poller) Close() error {
	p.dieOnce.Do(func() {
		close(p.die)
	})
	return p.wakeup()
}

// Watch adds a file descriptor to the list of awaiting-to-be-watched descriptors and wakes up the poller.
func (p *poller) Watch(fd int) error {
	p.watchQueueMutex.Lock()
	p.watchQueue = append(p.watchQueue, fd)
	p.watchQueueMutex.Unlock()

	return p.wakeup()
}

// wakeup interrupt kevent
func (p *poller) wakeup() error {
	p.mu.Lock()
	if p.fd != -1 {
		// notify poller
		_, err := syscall.Kevent(p.fd, []syscall.Kevent_t{{
			Ident:  0,
			Filter: syscall.EVFILT_USER,
			Fflags: syscall.NOTE_TRIGGER,
		}}, nil, nil)
		p.mu.Unlock()
		return err
	}
	p.mu.Unlock()
	return ErrPollerClosed
}

// Wait waits for events happen on the file descriptors.
func (p *poller) Wait(chSignal chan Signal) {
	var eventSet pollerEvents
	events := make([]syscall.Kevent_t, maxEvents)
	sig := Signal{
		done: make(chan struct{}, 1),
	}

	defer func() {
		p.mu.Lock()
		syscall.Close(p.fd)
		p.fd = -1
		p.mu.Unlock()
	}()

	// kqueue eventloop
	var changes []syscall.Kevent_t
	for {
		select {
		case <-p.die:
			return
		default:
			p.watchQueueMutex.Lock()
			for _, fd := range p.watchQueue {
				changes = append(changes,
					syscall.Kevent_t{Ident: uint64(fd), Flags: syscall.EV_ADD | syscall.EV_CLEAR, Filter: syscall.EVFILT_READ},
					syscall.Kevent_t{Ident: uint64(fd), Flags: syscall.EV_ADD | syscall.EV_CLEAR, Filter: syscall.EVFILT_WRITE},
				)
			}
			p.watchQueue = p.watchQueue[:0]
			p.watchQueueMutex.Unlock()

			// Set affinity if cpuid is set
			if cpuid := atomic.LoadInt32(&p.cpuid); cpuid != -1 {
				setAffinity(cpuid)
				atomic.StoreInt32(&p.cpuid, -1)
			}

			// poll
			n, err := syscall.Kevent(p.fd, changes, events, nil)
			if err == syscall.EINTR {
				continue
			}
			if err != nil {
				return
			}
			changes = changes[:0]

			// event processing
			for i := 0; i < n; i++ {
				ev := &events[i]
				if ev.Ident != 0 {
					e := event{ident: int(ev.Ident)}
					if ev.Filter == syscall.EVFILT_READ {
						e.ev |= EV_READ
						// https://golang.org/src/runtime/netpoll_kqueue.go
						// On some systems when the read end of a pipe
						// is closed the write end will not get a
						// _EVFILT_WRITE event, but will get a
						// _EVFILT_READ event with EV_EOF set.
						// Note that setting 'w' here just means that we
						// will wake up a goroutine waiting to write;
						// that goroutine will try the write again,
						// and the appropriate thing will happen based
						// on what that write returns (success, EPIPE, EAGAIN).
						if ev.Flags&syscall.EV_EOF != 0 {
							e.ev |= EV_WRITE
						}
					} else if ev.Filter == syscall.EVFILT_WRITE {
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

// raw read for nonblocking op to avert context switch
// NOTE:
//  1. we need to make sure that the fd has O_NONBLOCK set.
//  2. use RawSyscall to avoid context switch
func rawRead(fd int, p []byte) (n int, err error) {
	var _p0 unsafe.Pointer
	if len(p) > 0 {
		_p0 = unsafe.Pointer(&p[0])
	} else {
		_p0 = unsafe.Pointer(&_zero)
	}
	r0, _, e1 := syscall.RawSyscall(syscall.SYS_READ, uintptr(fd), uintptr(_p0), uintptr(len(p)))
	n = int(r0)
	if e1 != 0 {
		err = e1
	}
	return
}

// raw write for nonblocking op to avert context switch
func rawWrite(fd int, p []byte) (n int, err error) {
	var _p0 unsafe.Pointer
	if len(p) > 0 {
		_p0 = unsafe.Pointer(&p[0])
	} else {
		_p0 = unsafe.Pointer(&_zero)
	}
	r0, _, e1 := syscall.RawSyscall(syscall.SYS_WRITE, uintptr(fd), uintptr(_p0), uintptr(len(p)))
	n = int(r0)
	if e1 != 0 {
		err = e1
	}
	return
}
