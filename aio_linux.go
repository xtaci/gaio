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

// Handle represents one unique number for a watched connection
type Handle int

// aiocb contains all info for a request
type aiocb struct {
	fd     Handle
	buffer []byte
	size   int
	done   chan Result
}

// Result of operation
type Result struct {
	fd   Handle
	size int
	err  error
}

type handler struct {
	conn    net.Conn
	rawConn syscall.RawConn

	rcb aiocb
	wcb aiocb
	sync.Mutex
}

// Watcher will monitor & process Request(s)
type Watcher struct {
	rfd          int // epollin & epollout
	wfd          int
	handlers     map[Handle]*handler
	handlersLock sync.Mutex
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

	w.handlers = make(map[Handle]*handler)

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
func (w *Watcher) Watch(conn net.Conn) (fd Handle, err error) {
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
		fd = Handle(s)
	}); err != nil {
		return 0, err
	}
	if operr != nil {
		return 0, operr
	}

	h := handler{conn: conn, rawConn: rawconn}
	w.handlersLock.Lock()
	w.handlers[fd] = &h
	w.handlersLock.Unlock()

	return fd, nil
}

// StopWatch events on connection `conn`
func (w *Watcher) StopWatch(Fd Handle) (err error) {
	return nil
}

// Read submits a read requests to Handle
func (w *Watcher) Read(fd Handle, buf []byte, done chan Result) error {
	cb := aiocb{fd: fd, buffer: buf, done: done}
	w.handlersLock.Lock()
	h := w.handlers[fd]
	w.handlersLock.Unlock()

	if h != nil {
		h.Lock()
		h.rcb = cb
		syscall.EpollCtl(w.rfd, syscall.EPOLL_CTL_ADD, int(fd), &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLIN})
		h.Unlock()
		return nil
	}
	return errNotWatched
}

// Write submits a write requests to Handle
func (w *Watcher) Write(fd Handle, buf []byte, done chan Result) error {
	cb := aiocb{fd: fd, buffer: buf, done: done}
	w.handlersLock.Lock()
	h := w.handlers[fd]
	w.handlersLock.Unlock()

	if h != nil {
		h.Lock()
		h.wcb = cb
		syscall.EpollCtl(w.wfd, syscall.EPOLL_CTL_ADD, int(fd), &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLOUT})
		h.Unlock()
		return nil
	}
	return errNotWatched
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
			w.handlersLock.Lock()
			h := w.handlers[Handle(events[i].Fd)]
			w.handlersLock.Unlock()

			h.Lock()
			cb := h.rcb
			nr, er := syscall.Read(int(events[i].Fd), cb.buffer[:])
			if er == syscall.EAGAIN {
				h.Unlock()
				continue
			}

			syscall.EpollCtl(w.rfd, syscall.EPOLL_CTL_DEL, int(events[i].Fd), &syscall.EpollEvent{Fd: events[i].Fd, Events: syscall.EPOLLIN})
			if cb.done != nil {
				cb.done <- Result{fd: cb.fd, size: nr, err: er}
			}
			h.Unlock()
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
			w.handlersLock.Lock()
			h := w.handlers[Handle(events[i].Fd)]
			w.handlersLock.Unlock()

			h.Lock()
			nw, ew := syscall.Write(int(events[i].Fd), h.wcb.buffer)
			if ew == syscall.EAGAIN {
				h.Unlock()
				continue
			}
			if ew == nil {
				h.wcb.size += nw
				h.wcb.buffer = h.wcb.buffer[nw:]
			}

			if len(h.wcb.buffer) == 0 || ew != nil {
				syscall.EpollCtl(w.wfd, syscall.EPOLL_CTL_DEL, int(events[i].Fd), &syscall.EpollEvent{Fd: events[i].Fd, Events: syscall.EPOLLOUT})
				if h.wcb.done != nil {
					h.wcb.done <- Result{fd: h.wcb.fd, size: h.wcb.size, err: err}
				}
			}
			h.Unlock()
		}
	}
}
