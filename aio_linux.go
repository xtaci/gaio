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

// ReadRequest defines a single request for reading data
type Request struct {
	Fd         Handle
	Buffer     []byte
	OnComplete func(req Result) Action
}

type Result struct {
	Fd   Handle
	Size int
}

type handler struct {
	conn     net.Conn
	rawConn  syscall.RawConn
	reqRead  Request
	reqWrite Request
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

// Read submits a read requests to `conn`
func (w *Watcher) Read(req Request) error {
	w.handlersLock.Lock()
	h := w.handlers[req.Fd]
	w.handlersLock.Unlock()

	// request check
	if req.OnComplete == nil {
		return errOnCompleteIsNil
	}

	if h != nil {
		h.Lock()
		h.reqRead = req
		syscall.EpollCtl(w.rfd, syscall.EPOLL_CTL_ADD, int(req.Fd), &syscall.EpollEvent{Fd: int32(req.Fd), Events: syscall.EPOLLIN})
		h.Unlock()
		return nil
	}
	return errNotWatched
}

// Write submits a write requests to `conn`
func (w *Watcher) Write(wreq Request) error {
	w.handlersLock.Lock()
	h := w.handlers[wreq.Fd]
	w.handlersLock.Unlock()

	// request check
	if wreq.OnComplete == nil {
		return errOnCompleteIsNil
	}

	if h != nil {
		h.Lock()
		h.reqWrite = wreq
		syscall.EpollCtl(w.wfd, syscall.EPOLL_CTL_ADD, int(wreq.Fd), &syscall.EpollEvent{Fd: int32(wreq.Fd), Events: syscall.EPOLLOUT})
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
			req := h.reqRead
			h.Unlock()

			nr, er := syscall.Read(int(events[i].Fd), req.Buffer)
			if er == syscall.EAGAIN {
				continue
			}

			// callback
			action := req.OnComplete(Result{req.Fd, nr})

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
			req := h.reqWrite
			h.Unlock()

			nw, er := syscall.Write(int(events[i].Fd), req.Buffer)
			if er == syscall.EAGAIN {
				h.Unlock()
				continue
			}

			// callback
			action := req.OnComplete(Result{req.Fd, nw})

			switch action {
			case Remove:
				syscall.EpollCtl(w.wfd, syscall.EPOLL_CTL_DEL, int(events[i].Fd), &syscall.EpollEvent{Fd: events[i].Fd, Events: syscall.EPOLLOUT})
			case Keep:
			}
		}
	}
}
