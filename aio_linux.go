// +build linux

package gaio

import "golang.org/x/sys/unix"

type poller struct {
	pfd int // epoll fd
}

func OpenPoll() (*poller, error) {
	fd, err := unix.EpollCreate1(0)
	if err != nil {
		return nil, err
	}
	p := new(poller)
	p.pfd = fd

	return p, err
}

func (p *poller) Close() error { return unix.Close(p.pfd) }

func (p *poller) Watch(fd int) error {
	return unix.EpollCtl(p.pfd, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{Fd: int32(fd), Events: unix.EPOLLIN | unix.EPOLLOUT | unix.EPOLLET})
}

func (p *poller) Unwatch(fd int) error {
	return unix.EpollCtl(p.pfd, unix.EPOLL_CTL_DEL, fd, &unix.EpollEvent{Fd: int32(fd), Events: unix.EPOLLIN | unix.EPOLLOUT})
}

func (p *poller) Wait(chReadableNotify chan int, chWriteableNotify chan int, die chan struct{}) error {
	events := make([]unix.EpollEvent, 64)
	for {
		n, err := unix.EpollWait(p.pfd, events, -1)
		if err != nil && err != unix.EINTR {
			return err
		}

		for i := 0; i < n; i++ {
			if events[i].Events&unix.EPOLLIN > 0 {
				select {
				case chReadableNotify <- int(events[i].Fd):
				case <-die:
				}

			}
			if events[i].Events&unix.EPOLLOUT > 0 {
				select {
				case chWriteableNotify <- int(events[i].Fd):
				case <-die:
				}
			}
		}
	}
}
