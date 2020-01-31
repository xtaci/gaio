// +build linux

package gaio

import (
	"syscall"
)

// EPOLLET value is incorrect in syscall
const EPOLLET = 0x80000000

type poller struct {
	pfd int // epoll fd
}

func openPoll() (*poller, error) {
	fd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}

	p := new(poller)
	p.pfd = fd

	return p, err
}

func (p *poller) Close() error {
	return syscall.Close(p.pfd)
}

func (p *poller) Watch(fd int) error {
	// epoll_wait(2)
	// While one thread is blocked in a call to epoll_pwait(), it is possible for
	// another thread  to  add  a  file  descriptor  to  the  waited-upon  epoll
	// instance.  If the new file descriptor becomes ready, it will cause the
	// epoll_wait() call to unblock.
	return syscall.EpollCtl(p.pfd, syscall.EPOLL_CTL_ADD, int(fd), &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLRDHUP | syscall.EPOLLIN | syscall.EPOLLOUT | EPOLLET})
}

func (p *poller) Wait(chEventNotify chan pollerEvents, die chan struct{}) {
	events := make([]syscall.EpollEvent, maxEvents)
	swapEvents := make([]pollerEvents, 2)
	swapIdx := 0
	for i := 0; i < len(swapEvents); i++ {
		swapEvents[i] = make([]event, 0, maxEvents)
	}

	for {
		n, err := syscall.EpollWait(p.pfd, events, -1)
		if err != nil && err != syscall.EINTR {
			return
		}

		// note chan swap must not continue unexpected
		pe := swapEvents[swapIdx]
		pe = pe[:0]
		swapIdx = (swapIdx + 1) % len(swapEvents)

		for i := 0; i < n; i++ {
			ev := &events[i]
			e := event{ident: int(ev.Fd)}

			// EPOLLRDHUP (since Linux 2.6.17)
			// Stream socket peer closed connection, or shut down writing
			// half of connection.  (This flag is especially useful for writ-
			// ing simple code to detect peer shutdown when using Edge Trig-
			// gered monitoring.)
			if ev.Events&(syscall.EPOLLIN|syscall.EPOLLERR|syscall.EPOLLHUP|syscall.EPOLLRDHUP) != 0 {
				e.r = true
			}
			if ev.Events&(syscall.EPOLLOUT|syscall.EPOLLERR|syscall.EPOLLHUP) != 0 {
				e.w = true
			}
			pe = append(pe, e)
		}

		select {
		case chEventNotify <- pe:
		case <-die:
			return
		}
	}
}
