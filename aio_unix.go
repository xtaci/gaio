// +build aix darwin dragonfly freebsd linux netbsd openbsd solaris

package ev

import (
	"errors"
	"net"
	"sync"
	"syscall"
)

var (
	errRawConn    = errors.New("net.Conn does implement net.RawConn")
	errNotWatched = errors.New("connection is not being watched")
)

// Events defines callback functions when Async-IO can be completed
type Events struct {
	OnRead  func(c net.Conn, in []byte, err error)
	OnClose func(c net.Conn, err error)
}

// WriteRequest defines a writing requests
type WriteRequest struct {
	Out            []byte
	WriteCompleted func(c net.Conn, numWritten int, err error)
}

type handler struct {
	conn           net.Conn
	rawConn        syscall.RawConn
	events         Events
	writeRequests  []WriteRequest
	chWriteRequets chan struct{}
	sync.Mutex
}

// Watcher will monitor all events specified in Events struct for each connection
type Watcher struct {
	chReadBuffers chan []byte // shared read buffers
	handlers      sync.Map
}

// NewWatcher creates a management object for monitoring events of net.Conn
func NewWatcher(bufferCount int, bufSize int) *Watcher {
	w := new(Watcher)
	w.chReadBuffers = make(chan []byte, bufferCount)
	for i := 0; i < bufferCount; i++ {
		w.chReadBuffers <- make([]byte, bufSize)
	}
	return w
}

// Close stops monitoring on events for all connections
func (w *Watcher) Close() error {
	return nil
}

// Watch events `ev` for connection `conn`
func (w *Watcher) Watch(conn net.Conn, ev Events) error {
	c, ok := conn.(interface {
		SyscallConn() (syscall.RawConn, error)
	})

	if !ok {
		return errRawConn
	}

	rawconn, err := c.SyscallConn()
	if err != nil {
		return err
	}

	h := handler{conn: conn, rawConn: rawconn, events: ev, chWriteRequets: make(chan struct{}, 1)}
	w.handlers.Store(conn.RemoteAddr(), &h)

	go w.loopRead(&h)
	go w.loopWrite(&h)
	return nil
}

// Write submits a write requests to `conn`
func (w *Watcher) Write(conn net.Conn, req WriteRequest) error {
	v, ok := w.handlers.Load(conn.RemoteAddr())
	if ok {
		h := v.(*handler)
		h.Lock()
		h.writeRequests = append(h.writeRequests, req)
		h.Unlock()

		w.notifyWriteRequest(h)
		return nil
	}
	return errNotWatched
}

func (w *Watcher) notifyWriteRequest(h *handler) {
	select {
	case h.chWriteRequets <- struct{}{}:
	default:
	}
}

func (w *Watcher) loopRead(h *handler) {
	var locked bool
	c := h.rawConn
	var buf []byte

	for {
		var er error
		var nr int
		rr := c.Read(func(s uintptr) bool {
			buf = <-w.chReadBuffers
			locked = true
			nr, er = syscall.Read(int(s), buf)
			if er == syscall.EAGAIN {
				w.chReadBuffers <- buf
				locked = false
				return false
			}
			return true
		})

		// read EOF
		if nr == 0 && er == nil {
			if h.events.OnClose != nil {
				h.events.OnClose(h.conn, nil)
			}
			break
		}

		if nr > 0 {
			if h.events.OnRead != nil {
				h.events.OnRead(h.conn, buf[0:nr], er)
			}
			w.chReadBuffers <- buf
			locked = false
		}

		if rr != nil {
			if h.events.OnClose != nil {
				h.events.OnClose(h.conn, rr)
			}
			break
		}
	}

	if locked {
		w.chReadBuffers <- buf
	}
}

func (w *Watcher) loopWrite(h *handler) {
	c := h.rawConn

	for {
		var er error
		var nw int
		var req WriteRequest
		h.Lock()
		if len(h.writeRequests) > 0 {
			req = h.writeRequests[0]
			h.writeRequests = h.writeRequests[1:]
			h.Unlock()
		} else {
			h.Unlock()
			<-h.chWriteRequets
			continue
		}

		rwe := c.Write(func(s uintptr) bool {
			nw, er = syscall.Write(int(s), req.Out)
			if er == syscall.EAGAIN {
				return false
			}
			return true
		})

		if req.WriteCompleted != nil {
			req.WriteCompleted(h.conn, nw, er)
		}

		if rwe != nil {
			if h.events.OnClose != nil {
				h.events.OnClose(h.conn, rwe)
			}
			break
		}
	}
}
