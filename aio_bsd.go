// +build darwin netbsd freebsd openbsd dragonfly

package gaio

import (
	"sync"
	"syscall"
)

type poller struct {
	fd      int
	changes []syscall.Kevent_t
	sync.Mutex
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

func (p *poller) Watch(fd int) error {
	p.Lock()
	p.changes = append(p.changes,
		syscall.Kevent_t{Ident: uint64(fd), Flags: syscall.EV_ADD | syscall.EV_CLEAR, Filter: syscall.EVFILT_READ},
		syscall.Kevent_t{Ident: uint64(fd), Flags: syscall.EV_ADD | syscall.EV_CLEAR, Filter: syscall.EVFILT_WRITE},
	)
	p.Unlock()
	return p.trigger()
}

func (p *poller) Unwatch(fd int) error {
	p.Lock()
	p.changes = append(p.changes,
		syscall.Kevent_t{Ident: uint64(fd), Flags: syscall.EV_DELETE, Filter: syscall.EVFILT_READ},
		syscall.Kevent_t{Ident: uint64(fd), Flags: syscall.EV_DELETE, Filter: syscall.EVFILT_WRITE},
	)
	p.Unlock()
	return p.trigger()
}

func (p *poller) Wait(chReadableNotify chan int, chWriteableNotify chan int, die chan struct{}) error {
	events := make([]syscall.Kevent_t, 128)
	for {
		p.Lock()
		changes := p.changes
		p.changes = p.changes[:0]
		p.Unlock()

		n, err := syscall.Kevent(p.fd, changes, events, nil)
		if err != nil && err != syscall.EINTR {
			return err
		}

		for i := 0; i < n; i++ {
			if events[i].Ident != 0 {
				if events[i].Filter == syscall.EVFILT_READ {
					select {
					case chReadableNotify <- int(events[i].Ident):
					case <-die:
					}

				}
				if events[i].Filter == syscall.EVFILT_WRITE {
					select {
					case chWriteableNotify <- int(events[i].Ident):
					case <-die:
					}
				}
			}
		}
	}
}
