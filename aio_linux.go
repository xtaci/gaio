// +build linux

package gaio

import (
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

/*
#define _GNU_SOURCE
#include <sched.h>
#include <pthread.h>

void lock_thread(int cpuid) {
        pthread_t tid;
        cpu_set_t cpuset;

        tid = pthread_self();
        CPU_ZERO(&cpuset);
        CPU_SET(cpuid, &cpuset);
    pthread_setaffinity_np(tid, sizeof(cpu_set_t), &cpuset);
}
*/
import "C"

var (
	globalIdxCpu uint32
)

func setAffinity() {
	idx := atomic.AddUint32(&globalIdxCpu, 1)
	idx %= uint32(runtime.NumCPU())

	runtime.LockOSThread()
	C.lock_thread(C.int(idx))
}

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

	// awaiting for poll
	awaiting      []int
	awaitingMutex sync.Mutex

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

			n, err := epollWait(p.pfd, events, -1)
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
				} else {
					e := event{ident: int(ev.Fd)}

					// EPOLLRDHUP (since Linux 2.6.17)
					// Stream socket peer closed connection, or shut down writing
					// half of connection.  (This flag is especially useful for writ-
					// ing simple code to detect peer shutdown when using Edge Trig-
					// gered monitoring.)
					if ev.Events&(syscall.EPOLLIN|syscall.EPOLLERR|syscall.EPOLLHUP|syscall.EPOLLRDHUP) != 0 {
						e.ev |= EV_READ
					}
					if ev.Events&(syscall.EPOLLOUT|syscall.EPOLLERR|syscall.EPOLLHUP) != 0 {
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
