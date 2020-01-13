// +build linux

package gaio

import (
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
	awaiting      []int
	awaitingMutex sync.Mutex
}

func openPoll() (*poller, error) {
	fd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
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

	return p, err
}

func (p *poller) Close() error {
	syscall.Close(p.efd)
	return syscall.Close(p.pfd)
}

func (p *poller) Watch(fd int) error {
	p.awaitingMutex.Lock()
	p.awaiting = append(p.awaiting, fd)
	p.awaitingMutex.Unlock()

	// interrupt epoll_wait
	var x uint64 = 1
	_, err := syscall.Write(p.efd, (*(*[8]byte)(unsafe.Pointer(&x)))[:])
	return err
}

func (p *poller) Wait(chEventNotify chan pollerEvents, die chan struct{}) {
	events := make([]syscall.EpollEvent, 64)
	for {
		// check for new awaiting
		p.awaitingMutex.Lock()
		for _, fd := range p.awaiting {
			syscall.EpollCtl(p.pfd, syscall.EPOLL_CTL_ADD, int(fd), &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLRDHUP | syscall.EPOLLIN | syscall.EPOLLOUT | EPOLLET})
		}
		p.awaiting = p.awaiting[:0]
		p.awaitingMutex.Unlock()

		n, err := syscall.EpollWait(p.pfd, events, -1)
		if err != nil && err != syscall.EINTR {
			return
		}

		var pe pollerEvents
		pe.events = make([]event, 0, n)
		pe.done = make(chan struct{})

		for i := 0; i < n; i++ {
			ev := &events[i]
			if int(ev.Fd) == p.efd {
				syscall.Read(p.efd, p.efdbuf) // simply consume
			} else {
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
				pe.events = append(pe.events, e)
			}
		}

		select {
		case chEventNotify <- pe:
		case <-die:
			return
		}

		select {
		case <-pe.done:
		case <-die:
			return
		}
	}
}
