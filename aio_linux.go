// +build linux

package gaio

import "golang.org/x/sys/unix"

type poller struct {
	pfd    int // epoll fd
	wakefd int
}

func OpenPoll() (*poller, error) {
	fd, err := unix.EpollCreate1(0)
	if err != nil {
		return nil, err
	}
	r0, _, e0 := unix.Syscall(unix.SYS_EVENTFD2, 0, 0, 0)
	if e0 != 0 {
		unix.Close(fd)
		return nil, e0
	}
	p := new(poller)
	p.pfd = fd
	p.wakefd = int(r0)

	return p, err
}

func (p *poller) Close() error {
	if err := unix.Close(p.wakefd); err != nil {
		return err
	}
	return unix.Close(p.pfd)
}

func (p *poller) Watch(fd int) error {
	return unix.EpollCtl(p.pfd, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{Fd: int32(fd), Events: unix.EPOLLIN | unix.EPOLLOUT | unix.EPOLLET})
}

func (p *poller) Unwatch(fd int) error {
	return unix.EpollCtl(p.pfd, unix.EPOLL_CTL_DEL, fd, &unix.EpollEvent{Fd: int32(fd), Events: unix.EPOLLIN | unix.EPOLLOUT})
}

func (p *poller) Wait(chReadableNotify chan int, chWriteableNotify chan int) error {
	events := make([]unix.EpollEvent, 64)
	for {
		n, err := unix.EpollWait(p.pfd, events, -1)
		if err != nil && err != unix.EINTR {
			return err
		}

		for i := 0; i < n; i++ {
			if events[i].Events&unix.EPOLLIN > 0 {
				chReadableNotify <- int(events[i].Fd)

			}
			if events[i].Events&unix.EPOLLOUT > 0 {
				chWriteableNotify <- int(events[i].Fd)
			}
		}
	}
}
