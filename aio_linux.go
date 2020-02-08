// +build linux

package gaio

import (
	"sync"
	"syscall"
	"unsafe"
)

// _EPOLLET value is incorrect in syscall
const _EPOLLET = 0x80000000

type poller struct {
	mu     sync.Mutex // mutex to protect fd closing
	pfd    int        // epoll fd
	efd    int        // eventfd
	efdbuf []byte

	// awaiting for poll
	awaiting      []int
	awaitingMutex sync.Mutex

	// closing signal
	die     chan struct{}
	dieOnce sync.Once
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
	p.die = make(chan struct{})

	return p, err
}

// Close the poller
func (p *poller) Close() error {
	p.dieOnce.Do(func() {
		close(p.die)
	})
	return p.wakeup()
}

func (p *poller) Watch(fd int) error {
	p.awaitingMutex.Lock()
	p.awaiting = append(p.awaiting, fd)
	p.awaitingMutex.Unlock()

	return p.wakeup()
}

// wakeup interrupt epoll_wait
func (p *poller) wakeup() error {
	p.mu.Lock()

	if p.efd != -1 {
		var x uint64 = 1
		_, err := syscall.Write(p.efd, (*(*[8]byte)(unsafe.Pointer(&x)))[:])
		p.mu.Unlock()
		return err
	}
	p.mu.Unlock()
	return ErrPollerClosed
}

func (p *poller) Wait(chEventNotify chan pollerEvents) {
	events := make([]syscall.EpollEvent, maxEvents)
	// close poller fd & eventfd in defer
	defer func() {
		p.mu.Lock()
		syscall.Close(p.pfd)
		syscall.Close(p.efd)
		p.pfd = -1
		p.efd = -1
		p.mu.Unlock()
	}()

	// epoll eventloop
	for {
		select {
		case <-p.die:
			return
		default:
			// check for new awaiting
			p.awaitingMutex.Lock()
			for _, fd := range p.awaiting {
				syscall.EpollCtl(p.pfd, syscall.EPOLL_CTL_ADD, int(fd), &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLRDHUP | syscall.EPOLLIN | syscall.EPOLLOUT | _EPOLLET})
			}
			p.awaiting = p.awaiting[:0]
			p.awaitingMutex.Unlock()

			n, err := syscall.EpollWait(p.pfd, events, -1)
			if err == syscall.EINTR {
				continue
			}
			if err != nil {
				return
			}

			// event processing
			var pe pollerEvents
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

					pe = append(pe, e)
				}
			}

			select {
			case chEventNotify <- pe:
			case <-p.die:
				return
			}
		}
	}
}
