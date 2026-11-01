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

// poller monitors registered sockets with kqueue.
type poller struct {
	cpuid int32
	mu    sync.Mutex // mutex to protect fd closure
	fd    int        // kqueue fd

	// Queue of fds to register with the poller.
	watchQueue      []int
	watchQueueMutex sync.Mutex
	// shutdown signal
	die     chan struct{}
	dieOnce sync.Once
}

// openPoll creates a kqueue instance and registers its user-event wakeup.
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

// Watch queues a socket for read/write monitoring and wakes the poller.
func (p *poller) Watch(fd int) error {
	p.watchQueueMutex.Lock()
	p.watchQueue = append(p.watchQueue, fd)
	p.watchQueueMutex.Unlock()
	return p.wakeup()
}

// wakeup interrupts kevent.
func (p *poller) wakeup() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fd == -1 {
		return ErrPollerClosed
	}
	// notify poller
	_, err := syscall.Kevent(p.fd, []syscall.Kevent_t{{
		Ident:  0,
		Filter: syscall.EVFILT_USER,
		Fflags: syscall.NOTE_TRIGGER,
	}}, nil, nil)
	return err
}

// Wait polls for socket readiness and sends event batches to the watcher loop.
func (p *poller) Wait(chSignal chan Signal) {
	// Pre-allocate event set with typical capacity to reduce allocations
	eventSet := make(pollerEvents, 0, maxEvents)
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

	// kqueue event loop - pre-allocate the changes slice
	changes := make([]syscall.Kevent_t, 0, 256)
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

			// Apply any pending CPU-affinity request before polling.
			if cpuid := atomic.LoadInt32(&p.cpuid); cpuid != -1 {
				setAffinity(cpuid)
				atomic.StoreInt32(&p.cpuid, -1)
			}

			// Register queued sockets and wait for readiness events.
			n, err := syscall.Kevent(p.fd, changes, events, nil)
			if err == syscall.EINTR {
				continue
			}
			if err != nil {
				return
			}
			changes = changes[:0]

			// Translate kqueue events into platform-independent readiness flags.
			for i := 0; i < n; i++ {
				ev := &events[i]
				if ev.Ident != 0 {
					e := event{ident: int(ev.Ident)}
					if ev.Flags&syscall.EV_ERROR != 0 {
						// Wake both sides on error to let upper layers handle cleanup.
						e.ev |= EV_READ | EV_WRITE
					} else if ev.Filter == syscall.EVFILT_READ {
						e.ev |= EV_READ
						// On some systems, closing a pipe's read end does not produce
						// EVFILT_WRITE; it produces EVFILT_READ|EV_EOF instead. Marking
						// the event writable wakes pending writers so they can retry
						// and handle the result (success, EPIPE, or EAGAIN).
						// https://go.dev/src/runtime/netpoll_kqueue.go
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

// rawRead issues a non-blocking read directly through the system call interface.
// The file descriptor must have O_NONBLOCK set. Using RawSyscall avoids the
// normal syscall path; on error, RawSyscall returns -1 in r0, represented as
// MaxUint when converted to int on 64-bit systems.
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

// rawWrite issues a non-blocking write directly through the system call interface.
// On error, RawSyscall returns -1 in r0, represented as MaxUint when converted
// to int on 64-bit systems.
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
