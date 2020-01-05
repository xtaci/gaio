// +build linux

package gaio

import "syscall"

// EPOLLET value is incorrect in syscall
const EPOLLET = 0x80000000

type poller struct {
	pfd int // epoll fd
}

func openPoll() (*poller, error) {
	fd, err := syscall.EpollCreate1(0)
	if err != nil {
		return nil, err
	}
	p := new(poller)
	p.pfd = fd

	return p, err
}

func (p *poller) Close() error { return syscall.Close(p.pfd) }

func (p *poller) Watch(fd int) error {
	return syscall.EpollCtl(p.pfd, syscall.EPOLL_CTL_ADD, fd, &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLIN | syscall.EPOLLOUT | EPOLLET})
}

func (p *poller) Unwatch(fd int) error {
	return syscall.EpollCtl(p.pfd, syscall.EPOLL_CTL_DEL, fd, &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLIN | syscall.EPOLLOUT | EPOLLET})
}

func (p *poller) Wait(chReadableNotify chan int, chWriteableNotify chan int, die chan struct{}) error {
	events := make([]syscall.EpollEvent, 64)
	for {
		n, err := syscall.EpollWait(p.pfd, events, -1)
		if err != nil && err != syscall.EINTR {
			return err
		}

		for i := 0; i < n; i++ {
			if events[i].Events&syscall.EPOLLIN > 0 {
				select {
				case chReadableNotify <- int(events[i].Fd):
				case <-die:
					return nil
				}

			}
			if events[i].Events&syscall.EPOLLOUT > 0 {
				select {
				case chWriteableNotify <- int(events[i].Fd):
				case <-die:
					return nil
				}
			}
		}
	}
}
