// +build darwin netbsd freebsd openbsd dragonfly

package gaio

import (
	"syscall"
)

type poller *pollerS
type pollerS struct {
	fd      int
	changes []syscall.Kevent_t
}

func createpoll() (poller, error) {
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

	p := new(pollerS)
	p.fd = fd
	return p, nil
}

func closepoll(p poller) error {
	return syscall.Close(p.fd)
}

func trigger(p poller) error {
	_, err := syscall.Kevent(p.fd, []syscall.Kevent_t{{
		Ident:  0,
		Filter: syscall.EVFILT_USER,
		Fflags: syscall.NOTE_TRIGGER,
	}}, nil, nil)
	return err
}

func poll_in(p poller, fd int) error {
	p.changes = append(p.changes,
		syscall.Kevent_t{
			Ident: uint64(fd), Flags: syscall.EV_ADD, Filter: syscall.EVFILT_READ,
		},
	)
	return trigger(p)
}

func poll_out(p poller, fd int) error {
	p.changes = append(p.changes,
		syscall.Kevent_t{
			Ident: uint64(fd), Flags: syscall.EV_ADD, Filter: syscall.EVFILT_WRITE,
		},
	)
	return trigger(p)
}

func poll_del(p poller, fd int) error {
	p.changes = append(p.changes,
		syscall.Kevent_t{
			Ident: uint64(fd), Flags: syscall.EV_DELETE, Filter: syscall.EVFILT_READ,
		},
		syscall.Kevent_t{
			Ident: uint64(fd), Flags: syscall.EV_DELETE, Filter: syscall.EVFILT_WRITE,
		},
	)
	return trigger(p)
}

func poll_wait(p poller, callback func(fd int)) error {
	events := make([]syscall.Kevent_t, 128)
	for {
		n, err := syscall.Kevent(p.fd, p.changes, events, nil)
		if err != nil && err != syscall.EINTR {
			return err
		}

		for i := 0; i < n; i++ {
			if events[i].Ident != 0 {
				callback(int(events[i].Ident))
			}
		}
	}
}
