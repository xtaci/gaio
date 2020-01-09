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

func (p *poller) Watch(conn net.Conn, rawconn syscall.RawConn) {
	p.awaitingMutex.Lock()
	p.awaiting = append(p.awaiting, connRawConn{conn, rawconn})
	p.awaitingMutex.Unlock()
	p.trigger()
}

func (p *poller) Wait(chReadableNotify chan net.Conn, chWriteableNotify chan net.Conn, chRemovedNotify chan net.Conn, die chan struct{}) {
	events := make([]syscall.Kevent_t, 128)
	var changes []syscall.Kevent_t
	for {
		p.awaitingMutex.Lock()
		for _, c := range p.awaiting {
			var operr error
			err := c.rawConn.Control(func(fd uintptr) {
				if _, ok := p.watching[int(fd)]; !ok {
					p.watching[int(fd)] = c.conn
					changes = append(changes,
						syscall.Kevent_t{Ident: uint64(fd), Flags: syscall.EV_ADD | syscall.EV_CLEAR, Filter: syscall.EVFILT_READ},
						syscall.Kevent_t{Ident: uint64(fd), Flags: syscall.EV_ADD | syscall.EV_CLEAR, Filter: syscall.EVFILT_WRITE},
					)
					// apply changes one by one to notify caller
					_, operr = syscall.Kevent(p.fd, changes, nil, nil)
					changes = changes[:0]
				}
			})

			// if cannot control rawsocket or kevent failed
			if err != nil || operr != nil {
				select {
				case chRemovedNotify <- c.conn:
				case <-die:
					return
				}
			}
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
				if conn, ok := p.watching[int(ev.Ident)]; ok {
					var notifyRead, notifyWrite, removeFd bool

					if ev.Filter == syscall.EVFILT_READ {
						notifyRead = true
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
							notifyWrite = true
						}
					} else if ev.Filter == syscall.EVFILT_WRITE {
						notifyWrite = true
					}

					//  error
					if ev.Flags&(syscall.EV_ERROR|syscall.EV_EOF) != 0 {
						notifyRead = true
						notifyWrite = true
						removeFd = true
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
						delete(p.watching, int(ev.Ident))
						// apply changes immediately
						changes = append(changes,
							syscall.Kevent_t{Ident: ev.Ident, Flags: syscall.EV_DELETE, Filter: syscall.EVFILT_READ},
							syscall.Kevent_t{Ident: ev.Ident, Flags: syscall.EV_DELETE, Filter: syscall.EVFILT_WRITE},
						)
						// if the socket called close(), kevent will return error
						// just ignore this error
						_, _ = syscall.Kevent(p.fd, changes, nil, nil)
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
