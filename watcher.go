package gaio

import (
	"errors"
	"net"
	"sync"
	"syscall"
)

var (
	errRawConn = errors.New("net.Conn does implement net.RawConn")
)

// aiocb contains all info for a request
type aiocb struct {
	fd     int
	buffer []byte
	size   int
	done   chan OpResult
}

// OpResult of operation
type OpResult struct {
	Fd     int
	Buffer []byte // the original committed buffer
	Size   int
	Err    error
}

// Watcher will monitor & process Request(s)
type Watcher struct {
	rfd poller // epollin & epollout
	wfd poller

	// handlers for a fd(s)
	readers map[int]aiocb
	writers map[int]aiocb

	readersLock sync.Mutex
	writersLock sync.Mutex

	// hold net.Conn to prevent from GC
	conns     map[int]net.Conn
	connsLock sync.Mutex
}

// CreateWatcher creates a management object for monitoring events of net.Conn
func CreateWatcher() (*Watcher, error) {
	w := new(Watcher)
	rfd, err := createpoll()
	if err != nil {
		return nil, err
	}
	w.rfd = rfd

	wfd, err := createpoll()
	if err != nil {
		return nil, err
	}
	w.wfd = wfd

	w.readers = make(map[int]aiocb)
	w.writers = make(map[int]aiocb)
	w.conns = make(map[int]net.Conn)

	go w.loopRead()
	go w.loopWrite()
	return w, nil
}

// Close stops monitoring on events for all connections
func (w *Watcher) Close() error {
	er := closepoll(w.rfd)
	ew := closepoll(w.wfd)
	if er != nil {
		return er
	}
	if ew != nil {
		return ew
	}
	return nil
}

// StopWatch dereferences net.Conn related to this fd
func (w *Watcher) StopWatch(fd int) {
	w.connsLock.Lock()
	defer w.connsLock.Unlock()
	delete(w.conns, fd)
}

// Read submits a read requests to Handle
func (w *Watcher) Read(fd int, buf []byte, done chan OpResult) error {
	cb := aiocb{fd: fd, buffer: buf, done: done}
	w.readersLock.Lock()
	w.readers[fd] = cb
	w.readersLock.Unlock()
	return poll_in(w.rfd, fd)
}

// Write submits a write requests to Handle
func (w *Watcher) Write(fd int, buf []byte, done chan OpResult) error {
	cb := aiocb{fd: fd, buffer: buf, done: done}
	w.writersLock.Lock()
	w.writers[fd] = cb
	w.writersLock.Unlock()
	return poll_out(w.wfd, fd)
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

	w.connsLock.Lock()
	w.conns[fd] = conn
	w.connsLock.Unlock()
	return fd, nil
}

func (w *Watcher) loopRead() {
	callback := func(fd int) {
		w.readersLock.Lock()
		cb := w.readers[fd]
		nr, er := syscall.Read(int(fd), cb.buffer[:])
		if er == syscall.EAGAIN {
			w.readersLock.Unlock()
			return
		}
		result := OpResult{Fd: cb.fd, Buffer: cb.buffer, Size: nr, Err: er}
		poll_delete_in(w.rfd, fd)
		w.readersLock.Unlock()

		if cb.done != nil {
			cb.done <- result
		}
	}

	poll_wait(w.rfd, callback)
}

func (w *Watcher) loopWrite() {
	callback := func(fd int) {
		w.writersLock.Lock()
		cb := w.writers[fd]
		nw, ew := syscall.Write(fd, cb.buffer)
		if ew == syscall.EAGAIN {
			w.writersLock.Unlock()
			return
		}

		if ew == nil {
			cb.size += nw
		}

		if len(cb.buffer) == cb.size || ew != nil { // done
			poll_delete_out(w.wfd, fd)
			w.writersLock.Unlock()

			if cb.done != nil {
				cb.done <- OpResult{Fd: cb.fd, Buffer: cb.buffer, Size: cb.size, Err: ew}
			}
		} else {
			w.writers[fd] = cb
			w.writersLock.Unlock()
		}
	}

	poll_wait(w.wfd, callback)
}
