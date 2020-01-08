// +build darwin netbsd freebsd openbsd dragonfly

package gaio

import (
	"net"
	"sync"
	"syscall"
)

type poller struct {
	fd       int
	changes  []syscall.Kevent_t
	watching sync.Map
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

func (p *poller) Watch(fd int, conn net.Conn) error {
	if _, loaded := p.watching.LoadOrStore(fd, conn); !loaded {
		p.Lock()
		p.changes = append(p.changes,
			syscall.Kevent_t{Ident: uint64(fd), Flags: syscall.EV_ADD | syscall.EV_CLEAR, Filter: syscall.EVFILT_READ},
			syscall.Kevent_t{Ident: uint64(fd), Flags: syscall.EV_ADD | syscall.EV_CLEAR, Filter: syscall.EVFILT_WRITE},
		)
		p.Unlock()
		return p.trigger()
	}
	return nil
}

func (p *poller) Wait(chReadableNotify chan net.Conn, chWriteableNotify chan net.Conn, die chan struct{}) error {
	events := make([]syscall.Kevent_t, 128)
	for {
		p.Lock()
		changes := make([]syscall.Kevent_t, len(p.changes))
		copy(changes, p.changes)
		p.changes = p.changes[:0]
		p.Unlock()

		n, err := syscall.Kevent(p.fd, changes, events, nil)
		if err != nil && err != syscall.EINTR {
			return err
		}

		for i := 0; i < n; i++ {
			if events[i].Ident != 0 {
				if conn, ok := p.watching.Load(int(events[i].Ident)); ok {
					var notifyRead, notifyWrite, removeFd bool

					if events[i].Flags&syscall.EV_EOF != 0 {
						notifyRead = true
						notifyWrite = true
						removeFd = true
					}
					if events[i].Filter == syscall.EVFILT_READ {
						notifyRead = true
					} else if events[i].Filter == syscall.EVFILT_WRITE {
						notifyWrite = true
					}

					if notifyRead {
						select {
						case chReadableNotify <- conn.(net.Conn):
						case <-die:
							return nil
						}

					}
					if notifyWrite {
						select {
						case chWriteableNotify <- conn.(net.Conn):
						case <-die:
							return nil
						}
					}

					if removeFd {
						p.watching.Delete(int(events[i].Ident))
					}
				}
			}
		}
	}
}
