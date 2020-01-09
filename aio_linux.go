// +build linux

package gaio

import (
	"io"
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

	// sequence id for unique identification of objects
	seqid int32

	// awaiting for poll
	awaiting      []idconn
	awaitingMutex sync.Mutex
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

func (p *poller) Watch(ident int32, rawconn syscall.RawConn) {
	p.awaitingMutex.Lock()
	p.awaiting = append(p.awaiting, idconn{ident, rawconn})
	p.awaitingMutex.Unlock()
	p.trigger()
}

func (p *poller) NextId() int32 {
	p.seqid++
	if p.seqid == int32(p.efd) { // special one
		p.seqid++
	}
	return p.seqid
}

func (p *poller) Wait(chEventNotify chan event, die chan struct{}) {
	events := make([]syscall.EpollEvent, 64)
	for {
		// check for new awaiting
		p.awaitingMutex.Lock()
		for _, c := range p.awaiting {
			c.rawconn.Control(func(fd uintptr) {
				syscall.EpollCtl(p.pfd, syscall.EPOLL_CTL_ADD, int(fd), &syscall.EpollEvent{Fd: c.ident, Events: syscall.EPOLLRDHUP | syscall.EPOLLIN | syscall.EPOLLOUT | EPOLLET})
			})
		}
		p.awaiting = p.awaiting[:0]
		p.awaitingMutex.Unlock()

		n, err := syscall.EpollWait(p.pfd, events, -1)
		if err != nil && err != syscall.EINTR {
			return
		}

		for i := 0; i < n; i++ {
			ev := &events[i]
			if int(ev.Fd) == p.efd {
				syscall.Read(p.efd, p.efdbuf) // simply consume
			} else {
				e := event{ident: ev.Fd}

				// EPOLLRDHUP (since Linux 2.6.17)
				// Stream socket peer closed connection, or shut down writing
				// half of connection.  (This flag is especially useful for writ-
				// ing simple code to detect peer shutdown when using Edge Trig-
				// gered monitoring.)
				if ev.Events&(syscall.EPOLLERR|syscall.EPOLLHUP|syscall.EPOLLRDHUP) != 0 {
					e.r = true
					e.w = true
					e.err = io.ErrClosedPipe
				}

				if ev.Events&syscall.EPOLLIN != 0 {
					e.r = true
				}
				if ev.Events&syscall.EPOLLOUT != 0 {
					e.w = true
				}

				//log.Println(e)
				select {
				case chEventNotify <- e:
				case <-die:
					return
				}
			}
		}
	}
}
