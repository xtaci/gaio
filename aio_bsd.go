// +build darwin netbsd freebsd openbsd dragonfly

package gaio

import (
	"net"
	"sync"
	"syscall"
)

type poller struct {
	fd int

	// awaiting for poll
	awaiting      []connRawConn
	awaitingMutex sync.Mutex

	// under watching
	watching map[int]net.Conn
}

type connRawConn struct {
	conn    net.Conn
	rawConn syscall.RawConn
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
	p.watching = make(map[int]net.Conn)
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

func (p *poller) Watch(conn net.Conn, rawconn syscall.RawConn) error {
	p.awaitingMutex.Lock()
	p.awaiting = append(p.awaiting, connRawConn{conn, rawconn})
	p.awaitingMutex.Unlock()
	return p.trigger()
}

func (p *poller) Wait(chReadableNotify chan net.Conn, chWriteableNotify chan net.Conn, chRemovedNotify chan net.Conn, die chan struct{}) {
	events := make([]syscall.Kevent_t, 128)
	var changes []syscall.Kevent_t
	for {
		p.awaitingMutex.Lock()
		for _, c := range p.awaiting {
			c.rawConn.Control(func(fd uintptr) {
				if _, ok := p.watching[int(fd)]; !ok {
					p.watching[int(fd)] = c.conn
					changes = append(changes,
						syscall.Kevent_t{Ident: uint64(fd), Flags: syscall.EV_ADD | syscall.EV_CLEAR, Filter: syscall.EVFILT_READ},
						syscall.Kevent_t{Ident: uint64(fd), Flags: syscall.EV_ADD | syscall.EV_CLEAR, Filter: syscall.EVFILT_WRITE},
					)
				}
			})
		}
		p.awaiting = p.awaiting[:0]
		p.awaitingMutex.Unlock()

		// apply changes and poll wait
		n, err := syscall.Kevent(p.fd, changes, events, nil)
		if err != nil && err != syscall.EINTR {
			return
		}
		// clear changes
		changes = changes[:0]

		for i := 0; i < n; i++ {
			if events[i].Ident != 0 {
				if conn, ok := p.watching[int(events[i].Ident)]; ok {
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
							return
						}

					}
					if notifyWrite {
						select {
						case chWriteableNotify <- conn.(net.Conn):
						case <-die:
							return
						}
					}

					if removeFd {
						delete(p.watching, int(events[i].Ident))
						// apply changes immediately
						changes = append(changes,
							syscall.Kevent_t{Ident: events[i].Ident, Flags: syscall.EV_DELETE, Filter: syscall.EVFILT_READ},
							syscall.Kevent_t{Ident: events[i].Ident, Flags: syscall.EV_DELETE, Filter: syscall.EVFILT_WRITE},
						)
						_, err := syscall.Kevent(p.fd, changes, nil, nil)
						if err != nil && err != syscall.EINTR {
							return
						}
						changes = changes[:0]

						// notify listener
						select {
						case chRemovedNotify <- conn.(net.Conn):
						case <-die:
							return
						}
					}
				}
			}
		}
	}
}
