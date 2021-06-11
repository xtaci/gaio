// +build linux

package gaio

import (
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// _EPOLLET value is incorrect in syscall
const (
	_EPOLLET      = 0x80000000
	_EFD_NONBLOCK = 0x800
)

type poller struct {
	poolGeneric
	mu     sync.Mutex // mutex to protect fd closing
	pfd    int        // epoll fd
	efd    int        // eventfd
	efdbuf []byte

	// closing signal
	die     chan struct{}
	dieOnce sync.Once
}

// dupconn use RawConn to dup() file descriptor
func dupconn(conn net.Conn) (newfd int, err error) {
	sc, ok := conn.(interface {
		SyscallConn() (syscall.RawConn, error)
	})
	if !ok {
		return -1, ErrUnsupported
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return -1, ErrUnsupported
	}

	// Control() guarantees the integrity of file descriptor
	ec := rc.Control(func(fd uintptr) {
		newfd, err = syscall.Dup(int(fd))
	})

	if ec != nil {
		return -1, ec
	}

	return
}

func openPoll() (*poller, error) {
	fd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}
	r0, _, e0 := syscall.Syscall(syscall.SYS_EVENTFD2, 0, _EFD_NONBLOCK, 0)
	if e0 != 0 {
		syscall.Close(fd)
		return nil, err
	}

	if err := syscall.EpollCtl(fd, syscall.EPOLL_CTL_ADD, int(r0),
		&syscall.EpollEvent{Fd: int32(r0),
			Events: syscall.EPOLLIN | _EPOLLET,
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
	p.cpuid = -1

	return p, err
}

// Close the poller
func (p *poller) Close() error {
	p.dieOnce.Do(func() {
		close(p.die)
	})
	return p.wakeup()
}

func (p *poller) Watch(fd int) (err error) {
	p.mu.Lock()
	err = syscall.EpollCtl(p.pfd, syscall.EPOLL_CTL_ADD, int(fd), &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLONESHOT | syscall.EPOLLRDHUP | syscall.EPOLLIN | syscall.EPOLLOUT | _EPOLLET})
	p.mu.Unlock()
	return
}

func (p *poller) Rearm(fd int, read bool, write bool) (err error) {
	p.mu.Lock()
	var flag uint32
	flag = syscall.EPOLLONESHOT | _EPOLLET
	if read {
		flag |= syscall.EPOLLIN | syscall.EPOLLRDHUP
	}
	if write {
		flag |= syscall.EPOLLOUT
	}

	err = syscall.EpollCtl(p.pfd, syscall.EPOLL_CTL_MOD, int(fd), &syscall.EpollEvent{Fd: int32(fd), Events: flag})
	p.mu.Unlock()
	return
}

// wakeup interrupt epoll_wait
func (p *poller) wakeup() error {
	p.mu.Lock()
	if p.efd != -1 {
		var x uint64 = 1
		// eventfd has set with EFD_NONBLOCK
		_, err := syscall.Write(p.efd, (*(*[8]byte)(unsafe.Pointer(&x)))[:])
		p.mu.Unlock()
		return err
	}
	p.mu.Unlock()
	return ErrPollerClosed
}

func (p *poller) Wait(chEventNotify chan pollerEvents) {
	p.initCache(cap(chEventNotify) + 2)
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

	const (
		rSet = syscall.EPOLLIN | syscall.EPOLLRDHUP
		wSet = syscall.EPOLLOUT
	)

	// epoll eventloop
	for {
		select {
		case <-p.die:
			return
		default:
			n, err := syscall.EpollWait(p.pfd, events, -1)
			if err == syscall.EINTR {
				continue
			}
			if err != nil {
				return
			}

			// load from cache
			pe := p.loadCache(n)
			// event processing
			for i := 0; i < n; i++ {
				ev := &events[i]
				if int(ev.Fd) == p.efd {
					syscall.Read(p.efd, p.efdbuf) // simply consume
					// check cpuid
					if cpuid := atomic.LoadInt32(&p.cpuid); cpuid != -1 {
						setAffinity(cpuid)
						atomic.StoreInt32(&p.cpuid, -1)
					}
				} else {
					e := event{ident: int(ev.Fd)}

					// EPOLLRDHUP (since Linux 2.6.17)
					// Stream socket peer closed connection, or shut down writing
					// half of connection.  (This flag is especially useful for writ-
					// ing simple code to detect peer shutdown when using Edge Trig-
					// gered monitoring.)
					if ev.Events&rSet != 0 {
						e.ev |= EV_READ
					}
					if ev.Events&wSet != 0 {
						e.ev |= EV_WRITE
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

// Errno values.
var (
	errEAGAIN error = syscall.EAGAIN
	errEINVAL error = syscall.EINVAL
	errENOENT error = syscall.ENOENT
)

// errnoErr returns common boxed Errno values, to prevent
// allocations at runtime.
func errnoErr(e syscall.Errno) error {
	switch e {
	case 0:
		return nil
	case syscall.EAGAIN:
		return errEAGAIN
	case syscall.EINVAL:
		return errEINVAL
	case syscall.ENOENT:
		return errENOENT
	}
	return e
}

var _zero uintptr

// raw read for nonblocking op to avert context switch
func rawRead(fd int, p []byte) (n int, err error) {
	var _p0 unsafe.Pointer
	if len(p) > 0 {
		_p0 = unsafe.Pointer(&p[0])
	} else {
		_p0 = unsafe.Pointer(&_zero)
	}
	r0, _, e1 := syscall.RawSyscall(syscall.SYS_READ, uintptr(fd), uintptr(_p0), uintptr(len(p)))
	n = int(r0)
	if e1 != 0 {
		err = errnoErr(e1)
	}
	return
}

// raw write for nonblocking op to avert context switch
func rawWrite(fd int, p []byte) (n int, err error) {
	var _p0 unsafe.Pointer
	if len(p) > 0 {
		_p0 = unsafe.Pointer(&p[0])
	} else {
		_p0 = unsafe.Pointer(&_zero)
	}
	r0, _, e1 := syscall.RawSyscall(syscall.SYS_WRITE, uintptr(fd), uintptr(_p0), uintptr(len(p)))
	n = int(r0)
	if e1 != 0 {
		err = errnoErr(e1)
	}
	return
}
