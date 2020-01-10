// +build darwin netbsd freebsd openbsd dragonfly

package gaio

import (
	"sync"
	"syscall"
)

type poller struct {
	fd    int
	seqid int32
	// awaiting for poll
	awaiting      []int
	awaitingMutex sync.Mutex
}

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
	return p, nil
}

func (p *poller) Close() error { return syscall.Close(p.fd) }

func (p *poller) trigger() error {
	_, err := syscall.Kevent(p.fd, []syscall.Kevent_t{{
		Ident:  0,
		Filter: syscall.EVFILT_USER,
		Fflags: syscall.NOTE_TRIGGER,
	}}, nil, nil)
	return err
}

func (p *poller) Watch(fd int) {
	p.awaitingMutex.Lock()
	p.awaiting = append(p.awaiting, fd)
	p.awaitingMutex.Unlock()
	p.trigger()
}

func (p *poller) Wait(chEventNotify chan event, die chan struct{}) {
	events := make([]syscall.Kevent_t, 128)
	var changes []syscall.Kevent_t
	for {
		p.awaitingMutex.Lock()
		for _, fd := range p.awaiting {
			changes = append(changes,
				syscall.Kevent_t{Ident: uint64(fd), Flags: syscall.EV_ADD | syscall.EV_CLEAR, Filter: syscall.EVFILT_READ},
				syscall.Kevent_t{Ident: uint64(fd), Flags: syscall.EV_ADD | syscall.EV_CLEAR, Filter: syscall.EVFILT_WRITE},
			)
			// apply changes one by one to notify caller
			syscall.Kevent(p.fd, changes, nil, nil)
			changes = changes[:0]
		}
		p.awaiting = p.awaiting[:0]
		p.awaitingMutex.Unlock()

		// poll
		n, err := syscall.Kevent(p.fd, nil, events, nil)
		if err != nil && err != syscall.EINTR {
			return
		}

		for i := 0; i < n; i++ {
			ev := &events[i]
			if ev.Ident != 0 {
				e := event{ident: int(ev.Ident)}
				if ev.Filter == syscall.EVFILT_READ {
					e.r = true
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
						e.w = true
					}
				} else if ev.Filter == syscall.EVFILT_WRITE {
					e.w = true
				}

				//  error
				if ev.Flags&(syscall.EV_ERROR) != 0 {
					e.r = true
					e.w = true
					e.err = syscall.Errno(ev.Data)
				}

				select {
				case chEventNotify <- e:
				case <-die:
					return
				}
			}
		}
	}
}
