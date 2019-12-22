// +build linux

package gaio

import (
	"errors"
	"log"
	"net"
	"sync"
	"syscall"
)

var (
	errRawConn         = errors.New("net.Conn does implement net.RawConn")
	errNotWatched      = errors.New("connection is not being watched")
	errOnCompleteIsNil = errors.New("Request.OnComplete is nil")
)

// aiocb contains all info for a request
type aiocb struct {
	fd     int
	buffer []byte
	size   int
	done   chan OpResult
	sync.Mutex
}

// OpResult of operation
type OpResult struct {
	Fd   int
	Size int
	Err  error
}

// eventHandle holds control blocks for read & write
type eventHandle struct {
	rcb aiocb
	wcb aiocb
	sync.Mutex
}

// Watcher will monitor & process Request(s)
type Watcher struct {
	rfd int // epollin & epollout
	wfd int

	// handlers for a fd(s)
	readers map[int]aiocb
	writers map[int]aiocb

	readersLock sync.Mutex
	writersLock sync.Mutex
}

// CreateWatcher creates a management object for monitoring events of net.Conn
func CreateWatcher() (*Watcher, error) {
	w := new(Watcher)
	rfd, err := syscall.EpollCreate1(0)
	if err != nil {
		return nil, err
	}
	w.rfd = rfd

	wfd, err := syscall.EpollCreate1(0)
	if err != nil {
		return nil, err
	}
	w.wfd = wfd

	w.readers = make(map[int]aiocb)
	w.writers = make(map[int]aiocb)

	go w.loopRx()
	go w.loopTx()
	return w, nil
}

// Close stops monitoring on events for all connections
func (w *Watcher) Close() error {
	er := syscall.Close(w.rfd)
	ew := syscall.Close(w.wfd)
	if er != nil {
		return er
	}
	if ew != nil {
		return ew
	}
	return nil
}

// Watch starts watching events on connection `conn`
func (w *Watcher) Watch(conn net.Conn) (fd int, err error) {
	c, ok := conn.(interface {
		SyscallConn() (syscall.RawConn, error)
	})

	if !ok {
		return 0, errRawConn
	}

	rawconn, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}

	var operr error
	if err := rawconn.Control(func(s uintptr) {
		fd = int(s)
	}); err != nil {
		return 0, err
	}
	if operr != nil {
		return 0, operr
	}

	return fd, nil
}

// StopWatch events on connection `conn`
func (w *Watcher) StopWatch(Fd int) (err error) {
	return nil
}

// Read submits a read requests to Handle
func (w *Watcher) Read(fd int, buf []byte, done chan OpResult) error {
	cb := aiocb{fd: fd, buffer: buf, done: done}
	w.readersLock.Lock()
	w.readers[fd] = cb
	w.readersLock.Unlock()
	return syscall.EpollCtl(w.rfd, syscall.EPOLL_CTL_ADD, int(fd), &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLIN})
}

// Write submits a write requests to Handle
func (w *Watcher) Write(fd int, buf []byte, done chan OpResult) error {
	cb := aiocb{fd: fd, buffer: buf, done: done}
	w.writersLock.Lock()
	w.writers[fd] = cb
	w.writersLock.Unlock()
	return syscall.EpollCtl(w.wfd, syscall.EPOLL_CTL_ADD, int(fd), &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLOUT})
}

func (w *Watcher) loopRx() {
	events := make([]syscall.EpollEvent, 64)
	for {
		n, err := syscall.EpollWait(w.rfd, events, -1)
		if err != nil && err != syscall.EINTR {
			log.Println(err)
			return
		}

		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)

			w.readersLock.Lock()
			cb := w.readers[fd]
			nr, er := syscall.Read(int(fd), cb.buffer[:])
			if er == syscall.EAGAIN {
				w.readersLock.Unlock()
				continue
			}
			result := OpResult{Fd: cb.fd, Size: nr, Err: er}
			w.readersLock.Unlock()

			// read complete or error
			syscall.EpollCtl(w.rfd, syscall.EPOLL_CTL_DEL, int(fd), &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLIN})

			if cb.done != nil {
				cb.done <- result
			}
		}
	}
}

func (w *Watcher) loopTx() {
	events := make([]syscall.EpollEvent, 64)
	for {
		n, err := syscall.EpollWait(w.wfd, events, -1)
		if err != nil && err != syscall.EINTR {
			log.Println(err)
			return
		}

		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)

			w.writersLock.Lock()
			cb := w.writers[fd]
			nw, ew := syscall.Write(int(fd), cb.buffer)
			if ew == syscall.EAGAIN {
				w.writersLock.Unlock()
				continue
			}

			if ew == nil {
				cb.size += nw
				cb.buffer = cb.buffer[nw:]
			}

			if len(cb.buffer) == 0 || ew != nil { // done
				w.writersLock.Unlock()

				syscall.EpollCtl(w.wfd, syscall.EPOLL_CTL_DEL, int(fd), &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLOUT})
				cb.done <- OpResult{Fd: cb.fd, Size: cb.size, Err: err}
				if cb.done != nil {
					cb.done <- OpResult{Fd: cb.fd, Size: cb.size, Err: ew}
				}
			} else {
				w.writers[fd] = cb
				w.writersLock.Unlock()
			}
		}
	}
}
