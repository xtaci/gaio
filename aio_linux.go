// +build linux

package gaio

import (
	"syscall"
)

type poller int

func createpoll() (poller, error) {
	fd, err := syscall.EpollCreate1(0)
	return poller(fd), err
}

func closepoll(p poller) error {
	return syscall.Close(int(p))
}

func poll_in(p poller, fd int) error {
	return syscall.EpollCtl(int(p), syscall.EPOLL_CTL_ADD, int(fd), &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLIN})
}

func poll_out(p poller, fd int) error {
	return syscall.EpollCtl(int(p), syscall.EPOLL_CTL_ADD, int(fd), &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLOUT})
}
func poll_del(p poller, fd int) error {
	return syscall.EpollCtl(int(p), syscall.EPOLL_CTL_DEL, int(fd), &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLIN | syscall.EPOLLOUT})
}

func poll_wait(p poller, callback func(fd int)) error {
	events := make([]syscall.EpollEvent, 64)
	for {
		n, err := syscall.EpollWait(int(p), events, -1)
		if err != nil && err != syscall.EINTR {
			return err
		}

		for i := 0; i < n; i++ {
			callback(int(events[i].Fd))
		}
	}
}
