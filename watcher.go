package gaio

import (
	"errors"
	"net"
	"sync"
	"syscall"
)

var (
	ErrNoRawConn     = errors.New("net.Conn does implement net.RawConn")
	ErrWatcherClosed = errors.New("watcher closed")
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

// Watcher will monitor events and process Request(s)
type Watcher struct {
	pfd *poller // poll fd

	chReadableNotify chan int
	chWritableNotify chan int
	chReaders        chan aiocb
	chWriters        chan aiocb

	die     chan struct{}
	dieOnce sync.Once

	// hold net.Conn to prevent from GC
	conns     map[int]net.Conn
	connsLock sync.Mutex
}

// CreateWatcher creates a management object for monitoring events of net.Conn
func CreateWatcher() (*Watcher, error) {
	w := new(Watcher)
	pfd, err := OpenPoll()
	if err != nil {
		return nil, err
	}
	w.pfd = pfd

	w.chReadableNotify = make(chan int)
	w.chWritableNotify = make(chan int)
	w.chReaders = make(chan aiocb)
	w.chWriters = make(chan aiocb)
	w.conns = make(map[int]net.Conn)
	w.die = make(chan struct{})

	go w.pfd.Wait(w.chReadableNotify, w.chWritableNotify, w.die)
	go w.loop()
	return w, nil
}

// Close stops monitoring on events for all connections
func (w *Watcher) Close() error {
	w.dieOnce.Do(func() {
		close(w.die)
	})
	return w.pfd.Close()
}

// Watch starts watching events on connection `conn`
func (w *Watcher) Watch(conn net.Conn) (fd int, err error) {
	c, ok := conn.(interface {
		SyscallConn() (syscall.RawConn, error)
	})

	if !ok {
		return 0, ErrNoRawConn
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

	// poll this fd
	w.pfd.Watch(fd)

	// prevent GC net.Conn
	w.connsLock.Lock()
	w.conns[fd] = conn
	w.connsLock.Unlock()
	return fd, nil
}

// StopWatch events related to this fd
func (w *Watcher) StopWatch(fd int) {
	w.pfd.Unwatch(fd)
	w.connsLock.Lock()
	defer w.connsLock.Unlock()
	delete(w.conns, fd)
}

// Read submits a read requests and notify with done
func (w *Watcher) Read(fd int, buf []byte, done chan OpResult) error {
	select {
	case w.chReaders <- aiocb{fd: fd, buffer: buf, done: done}:
		return nil
	case <-w.die:
		return ErrWatcherClosed
	}
}

// Write submits a write requests and notify with done
func (w *Watcher) Write(fd int, buf []byte, done chan OpResult) error {
	select {
	case w.chWriters <- aiocb{fd: fd, buffer: buf, done: done}:
		return nil
	case <-w.die:
		return ErrWatcherClosed
	}
}

func (w *Watcher) loop() {
	pendingReaders := make(map[int]aiocb)
	pendingWriters := make(map[int]aiocb)

	for {
		select {
		case cb := <-w.chReaders:
			nr, er := syscall.Read(cb.fd, cb.buffer)
			if er == syscall.EAGAIN {
				pendingReaders[cb.fd] = cb
				continue
			}
			cb.done <- OpResult{Fd: cb.fd, Buffer: cb.buffer, Size: nr, Err: er}
		case cb := <-w.chWriters:
			nw, ew := syscall.Write(cb.fd, cb.buffer)
			if ew == syscall.EAGAIN {
				pendingWriters[cb.fd] = cb
				continue
			}

			if nw == len(cb.buffer) || ew != nil {
				cb.done <- OpResult{Fd: cb.fd, Buffer: cb.buffer, Size: nw, Err: ew}
				continue
			}

			// unsent buffer
			cb.size = nw
			pendingWriters[cb.fd] = cb
		case fd := <-w.chReadableNotify:
			//log.Println(fd, "readn")
			cb, ok := pendingReaders[fd]
			if ok {
				nr, er := syscall.Read(cb.fd, cb.buffer)
				if er == syscall.EAGAIN {
					continue
				}
				delete(pendingReaders, fd)
				cb.done <- OpResult{Fd: fd, Buffer: cb.buffer, Size: nr, Err: er}
			}
		case fd := <-w.chWritableNotify:
			//log.Println(fd, "writen")
			cb, ok := pendingWriters[fd]
			if ok {
				nw, ew := syscall.Write(cb.fd, cb.buffer[cb.size:])
				if ew == syscall.EAGAIN {
					continue
				}

				if ew == nil {
					cb.size += nw
				}

				// return if write complete or error
				if cb.size == len(cb.buffer) || ew != nil {
					delete(pendingWriters, cb.fd)
					cb.done <- OpResult{Fd: fd, Buffer: cb.buffer, Size: cb.size, Err: ew}
					continue
				}
				pendingWriters[fd] = cb
			}
		case <-w.die:
			return
		}
	}
}
