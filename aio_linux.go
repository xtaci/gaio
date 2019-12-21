// +build linux

package ev

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

type Action int

// Action is an action that occurs after the completion of an request.
const (
	// Remove indicates that the request will be removed from queue
	Remove Action = iota
	// Keep this Request in the queue, useful for reading continuously
	Keep
)

// Handle represents one unique number for a watched connection
type Handle int

// Callback function prototype
type Callback func(fd Handle, size int, err error) Action

// controlBlock defines a AIO request context
type controlBlock struct {
	fd       Handle
	buffer   []byte
	callback Callback
}

type handler struct {
	conn     net.Conn
	rawConn  syscall.RawConn
	reqRead  controlBlock
	reqWrite controlBlock
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
func (w *Watcher) Read(fd Handle, buf []byte, callback Callback) error {
	cb := controlBlock{fd, buf, callback}
	w.handlersLock.Lock()
	h := w.handlers[fd]
	w.handlersLock.Unlock()

	if h != nil {
		h.Lock()
		h.reqRead = cb
		syscall.EpollCtl(w.rfd, syscall.EPOLL_CTL_ADD, int(fd), &syscall.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLIN})
		h.Unlock()
		return nil
	}
	return errNotWatched
}

// Write submits a write requests to Handle
func (w *Watcher) Write(fd Handle, buf []byte, callback Callback) error {
	cb := controlBlock{fd, buf, callback}
	w.handlersLock.Lock()
	h := w.handlers[fd]
	w.handlersLock.Unlock()

	if h != nil {
		h.Lock()
		h.reqWrite = cb
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
			cb := h.reqRead
			h.Unlock()

			nr, er := syscall.Read(int(events[i].Fd), cb.buffer)
			if er == syscall.EAGAIN {
				continue
			}

			// callback
			var action Action
			if cb.callback != nil {
				action = cb.callback(cb.fd, nr, er)
			}

			switch action {
			case Remove:
				syscall.EpollCtl(w.rfd, syscall.EPOLL_CTL_DEL, int(events[i].Fd), &syscall.EpollEvent{Fd: events[i].Fd, Events: syscall.EPOLLIN})
			case Keep:
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
			w.handlersLock.Lock()
			h := w.handlers[Handle(events[i].Fd)]
			w.handlersLock.Unlock()

			h.Lock()
			cb := h.reqWrite
			h.Unlock()

			nw, ew := syscall.Write(int(events[i].Fd), cb.buffer)
			if ew == syscall.EAGAIN {
				continue
			}

			// callback
			var action Action
			if cb.callback != nil {
				action = cb.callback(cb.fd, nw, ew)
			}

			switch action {
			case Remove:
				syscall.EpollCtl(w.wfd, syscall.EPOLL_CTL_DEL, int(events[i].Fd), &syscall.EpollEvent{Fd: events[i].Fd, Events: syscall.EPOLLOUT})
			case Keep:
			}
		}
	}
}
