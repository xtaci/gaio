// +build linux

package gaio

import (
	"syscall"
)

type poller *pollerS
type pollerS struct {
	pfd    int // epoll fd
	wakefd int
}

func createpoll() (poller, error) {
	fd, err := syscall.EpollCreate1(0)
	if err != nil {
		return nil, err
	}
	r0, _, e0 := syscall.Syscall(syscall.SYS_EVENTFD2, 0, 0, 0)
	if e0 != 0 {
		syscall.Close(fd)
		return nil, e0
	}
	p := new(pollerS)
	p.pfd = fd
	p.wakefd = int(r0)

	return p, err
}

func closepoll(p poller) error {
	if err := syscall.Close(p.wakefd); err != nil {
		return err
	}
	return syscall.Close(p.pfd)
}

func poll_in(p poller, fd int) error {
	return syscall.EpollCtl(p.pfd, syscall.EPOLL_CTL_ADD, fd, &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLIN})
}

func poll_out(p poller, fd int) error {
	return syscall.EpollCtl(p.pfd, syscall.EPOLL_CTL_ADD, fd, &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLOUT})
}
func poll_delete_in(p poller, fd int) error {
	return syscall.EpollCtl(p.pfd, syscall.EPOLL_CTL_DEL, fd, &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLIN})
}

func poll_delete_out(p poller, fd int) error {
	return syscall.EpollCtl(p.pfd, syscall.EPOLL_CTL_DEL, fd, &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLOUT})
}

func poll_wait(p poller, callback func(fd int)) error {
	events := make([]syscall.EpollEvent, 64)
	for {
		n, err := syscall.EpollWait(p.pfd, events, -1)
		if err != nil && err != syscall.EINTR {
			return err
		}

		for i := 0; i < n; i++ {
			callback(int(events[i].Fd))
		}
	}
}
