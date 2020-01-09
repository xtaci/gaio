// +build linux

package gaio

import (
	"net"
	"sync"
	"syscall"
	"unsafe"
)

// EPOLLET value is incorrect in syscall
const EPOLLET = 0x80000000

type poller struct {
	pfd    int // epoll fd
	efd    int // eventfd
	efdbuf []byte

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
	fd, err := syscall.EpollCreate1(0)
	if err != nil {
		return nil, err
	}
	r0, _, e0 := syscall.Syscall(syscall.SYS_EVENTFD2, 0, 0, 0)
	if e0 != 0 {
		syscall.Close(fd)
		return nil, err
	}

	if err := syscall.EpollCtl(fd, syscall.EPOLL_CTL_ADD, int(r0),
		&syscall.EpollEvent{Fd: int32(r0),
			Events: syscall.EPOLLIN,
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
	p.watching = make(map[int]net.Conn)

	return p, err
}

func (p *poller) Close() error {
	syscall.Close(p.efd)
	return syscall.Close(p.pfd)
}

func (p *poller) trigger() error {
	var x uint64 = 1
	_, err := syscall.Write(p.efd, (*(*[8]byte)(unsafe.Pointer(&x)))[:])
	return err
}

func (p *poller) Watch(conn net.Conn, rawconn syscall.RawConn) {
	p.awaitingMutex.Lock()
	p.awaiting = append(p.awaiting, connRawConn{conn, rawconn})
	p.awaitingMutex.Unlock()
	p.trigger()
}

func (p *poller) Wait(chReadableNotify chan net.Conn, chWriteableNotify chan net.Conn, chRemovedNotify chan net.Conn, die chan struct{}) {
	events := make([]syscall.EpollEvent, 64)
	for {
		// check for new awaiting
		p.awaitingMutex.Lock()
		for _, c := range p.awaiting {
			err := c.rawConn.Control(func(fd uintptr) {
				if _, ok := p.watching[int(fd)]; !ok {
					p.watching[int(fd)] = c.conn
					syscall.EpollCtl(p.pfd, syscall.EPOLL_CTL_ADD, int(fd), &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLRDHUP | syscall.EPOLLIN | syscall.EPOLLOUT | EPOLLET})
				}
			})
			// if cannot control rawsocket
			if err != nil {
				select {
				case chRemovedNotify <- c.conn:
				case <-die:
					return
				}
			}
		}
		p.awaiting = p.awaiting[:0]
		p.awaitingMutex.Unlock()

		n, err := syscall.EpollWait(p.pfd, events, -1)
		if err != nil && err != syscall.EINTR {
			return
		}

		for i := 0; i < n; i++ {
			if int(events[i].Fd) == p.efd {
				syscall.Read(p.efd, p.efdbuf) // simply consume
			} else if conn, ok := p.watching[int(events[i].Fd)]; ok {
				var notifyRead, notifyWrite, removeFd bool

				// EPOLLRDHUP (since Linux 2.6.17)
				// Stream socket peer closed connection, or shut down writing
				// half of connection.  (This flag is especially useful for writ-
				// ing simple code to detect peer shutdown when using Edge Trig-
				// gered monitoring.)
				if events[i].Events&syscall.EPOLLERR > 0 || events[i].Events&syscall.EPOLLRDHUP > 0 {
					notifyRead = true
					notifyWrite = true
					removeFd = true
				}
				if events[i].Events&syscall.EPOLLIN > 0 {
					notifyRead = true
				}
				if events[i].Events&syscall.EPOLLOUT > 0 {
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
					delete(p.watching, int(events[i].Fd))
					syscall.EpollCtl(p.pfd, syscall.EPOLL_CTL_DEL, int(events[i].Fd), &syscall.EpollEvent{Fd: int32(events[i].Fd), Events: syscall.EPOLLIN | syscall.EPOLLOUT | EPOLLET})
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
