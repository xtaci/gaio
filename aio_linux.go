// +build linux

package gaio

import (
	"net"
	"sync"
	"syscall"
)

// EPOLLET value is incorrect in syscall
const EPOLLET = 0x80000000

type poller struct {
	pfd      int // epoll fd
	watching map[int]net.Conn
	sync.Mutex
}

func openPoll() (*poller, error) {
	fd, err := syscall.EpollCreate1(0)
	if err != nil {
		return nil, err
	}
	p := new(poller)
	p.pfd = fd
	p.watching = make(map[int]net.Conn)

	return p, err
}

func (p *poller) Close() error { return syscall.Close(p.pfd) }

func (p *poller) Watch(conn net.Conn, rawconn syscall.RawConn) (err error) {
	p.Lock()
	rawconn.Control(func(fd uintptr) {
		if _, ok := p.watching[int(fd)]; !ok {
			p.watching[int(fd)] = conn
			syscall.EpollCtl(p.pfd, syscall.EPOLL_CTL_ADD, int(fd), &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLIN | syscall.EPOLLOUT | EPOLLET})
		}
	})
	p.Unlock()
	return
}

func (p *poller) Wait(chReadableNotify chan net.Conn, chWriteableNotify chan net.Conn, die chan struct{}) error {
	events := make([]syscall.EpollEvent, 64)
	for {
		n, err := syscall.EpollWait(p.pfd, events, -1)
		if err != nil && err != syscall.EINTR {
			return err
		}

		p.Lock()
		for i := 0; i < n; i++ {
			if conn, ok := p.watching[int(events[i].Fd)]; ok {
				var notifyRead, notifyWrite, removeFd bool

				if events[i].Events&syscall.EPOLLERR > 0 {
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
					delete(p.watching, int(events[i].Fd))
				}
			}
		}
		p.Unlock()
	}
}
