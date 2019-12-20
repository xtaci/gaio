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

// Asynchronous request
type Request struct {
	Fd          int
	Buffer      []byte
	NBytes      int
	Offset      int
	ReadPersist bool
	Error       error
	OnComplete  func(req *Request)
}

type handler struct {
	conn          net.Conn
	rawConn       syscall.RawConn
	readRequests  []*Request
	writeRequests []*Request
	sync.Mutex
}

// Watcher will monitor & process Request(s)
type Watcher struct {
	rfd          int // epollin & epollout
	wfd          int
	handlers     map[int]*handler
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

	w.handlers = make(map[int]*handler)

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

// Watch events `ev` for connection `conn`
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

	h := handler{conn: conn, rawConn: rawconn}
	w.handlersLock.Lock()
	w.handlers[fd] = &h
	w.handlersLock.Unlock()

	return fd, nil
}

// Read submits a read requests to `conn`
func (w *Watcher) Read(req *Request) error {
	w.handlersLock.Lock()
	h := w.handlers[req.Fd]
	w.handlersLock.Unlock()

	// request check
	if req.OnComplete == nil {
		return errOnCompleteIsNil
	}

	if h != nil {
		h.Lock()
		syscall.EpollCtl(w.rfd, syscall.EPOLL_CTL_ADD, int(req.Fd), &syscall.EpollEvent{Fd: int32(req.Fd), Events: syscall.EPOLLIN})
		h.readRequests = append(h.readRequests, req)
		h.Unlock()
		return nil
	}
	return errNotWatched
}

// Write submits a write requests to `conn`
func (w *Watcher) Write(req *Request) error {
	w.handlersLock.Lock()
	h := w.handlers[req.Fd]
	w.handlersLock.Unlock()

	// request check
	if req.OnComplete == nil {
		return errOnCompleteIsNil
	}

	if h != nil {
		h.Lock()
		syscall.EpollCtl(w.wfd, syscall.EPOLL_CTL_ADD, int(req.Fd), &syscall.EpollEvent{Fd: int32(req.Fd), Events: syscall.EPOLLOUT})
		h.writeRequests = append(h.writeRequests, req)
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
			h := w.handlers[int(events[i].Fd)]
			w.handlersLock.Unlock()

			h.Lock()
			if len(h.readRequests) > 0 {
				req := h.readRequests[0]
				nr, er := syscall.Read(int(events[i].Fd), req.Buffer)
				if er == syscall.EAGAIN {
					h.Unlock()
					continue
				}

				if !req.ReadPersist {
					h.readRequests = h.readRequests[1:]
					if len(h.readRequests) == 0 {
						syscall.EpollCtl(w.rfd, syscall.EPOLL_CTL_DEL, int(events[i].Fd), &syscall.EpollEvent{Fd: events[i].Fd, Events: syscall.EPOLLIN})
					}
				}
				h.Unlock()

				req.NBytes = nr
				req.OnComplete(req)
			} else {
				h.Unlock()
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
			h := w.handlers[int(events[i].Fd)]
			w.handlersLock.Unlock()

			h.Lock()
			if len(h.writeRequests) > 0 {
				req := h.writeRequests[0]
				nw, er := syscall.Write(int(events[i].Fd), req.Buffer)
				if er == syscall.EAGAIN {
					h.Unlock()
					continue
				}

				h.writeRequests = h.writeRequests[1:]
				if len(h.writeRequests) == 0 {
					syscall.EpollCtl(w.wfd, syscall.EPOLL_CTL_DEL, int(events[i].Fd), &syscall.EpollEvent{Fd: events[i].Fd, Events: syscall.EPOLLOUT})
				}
				h.Unlock()

				req.NBytes = nw
				req.OnComplete(req)
			} else {
				h.Unlock()
			}
		}
	}
}
