// Package gaio is an Async-IO library for Golang.
//
// gaio acts in proactor mode, https://en.wikipedia.org/wiki/Proactor_pattern.
// User submit async IO operations and waits for IO-completion signal.
package gaio

import (
	"container/list"
	"errors"
	"net"
	"sync"
	"syscall"
	"time"
)

var (
	ErrNoRawConn     = errors.New("net.Conn does implement net.RawConn")
	ErrWatcherClosed = errors.New("watcher closed")
)

// Operation Type
type OpType int

const (
	// identify operation type on OpResult
	OpRead OpType = iota
	OpWrite
)

// aiocb contains all info for a request
type aiocb struct {
	ctx      interface{}
	fd       int
	buffer   []byte
	internal bool // mark if the buffer is internal
	size     int
	deadline time.Time
}

// OpResult is the result of an aysnc-io
type OpResult struct {
	// Operation Type
	Op OpType
	// A Context along with requests
	Context interface{}
	// Related file descriptor to this result
	Fd int
	// If the operation is Write, buffer is the original committed one,
	// if the operation is Read, buffer points to a internal buffer, you need
	// to process immediately, or copy and save by yourself.
	Buffer []byte
	// Number of bytes sent or received, Buffer[:Size] is the content sent or received.
	Size int
	// IO error
	Err error
}

// Watcher will monitor events and process async-io request(s),
type Watcher struct {
	pfd *poller // poll fd

	// loop
	chReadableNotify  chan int
	chWritableNotify  chan int
	chStopWatchNotify chan int
	chPendingNotify   chan struct{}
	chIOCompletion    chan OpResult

	// lock for pending io operations
	// aiocb is associated to fd
	pendingReaders map[int][]*aiocb
	pendingWriters map[int][]*aiocb
	pendingMutex   sync.Mutex

	// internal buffer for reading
	swapBuffer chan []byte

	die     chan struct{}
	dieOnce sync.Once

	// hold net.Conn to prevent from GC
	conns      map[int]net.Conn
	connsMutex sync.Mutex
}

// CreateWatcher creates a management object for monitoring file descriptors
func CreateWatcher(bufsize int) (*Watcher, error) {
	w := new(Watcher)
	pfd, err := openPoll()
	if err != nil {
		return nil, err
	}
	w.pfd = pfd

	// swapBuffer for concurrent read
	w.swapBuffer = make(chan []byte, 2)
	for i := 0; i < cap(w.swapBuffer); i++ {
		w.swapBuffer <- make([]byte, bufsize)
	}

	// loop related chan
	w.chReadableNotify = make(chan int)
	w.chWritableNotify = make(chan int)
	w.chStopWatchNotify = make(chan int)
	w.chPendingNotify = make(chan struct{}, 1)
	w.chIOCompletion = make(chan OpResult)

	// loop related map
	w.pendingReaders = make(map[int][]*aiocb)
	w.pendingWriters = make(map[int][]*aiocb)

	// hold net.Conn only
	w.conns = make(map[int]net.Conn)
	w.die = make(chan struct{})

	go w.pfd.Wait(w.chReadableNotify, w.chWritableNotify, w.die)
	go w.loop()
	return w, nil
}

// Close stops monitoring on events for all connections
func (w *Watcher) Close() (err error) {
	w.dieOnce.Do(func() {
		close(w.die)
		err = w.pfd.Close()
	})
	return err
}

// Watch starts watching events on `conn`, and returns a file descriptor
// for following IO operations.
func (w *Watcher) Watch(conn net.Conn) (fd int, err error) {
	// get file descriptor
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

	// prevent conn from GC
	w.connsMutex.Lock()
	w.conns[fd] = conn
	w.connsMutex.Unlock()
	return fd, nil
}

// StopWatch events related to this fd
func (w *Watcher) StopWatch(fd int) {
	w.pfd.Unwatch(fd)
	w.connsMutex.Lock()
	delete(w.conns, fd)
	w.connsMutex.Unlock()

	select {
	case w.chStopWatchNotify <- fd:
	case <-w.die:
	}
}

// notify new operations pending
func (w *Watcher) notifyPending() {
	select {
	case w.chPendingNotify <- struct{}{}:
	default:
	}
}

// Wait blocks until any read/write completion, or error
func (w *Watcher) WaitIO() (r OpResult, err error) {
	select {
	case r := <-w.chIOCompletion:
		return r, nil
	case <-w.die:
		return r, ErrWatcherClosed
	}
}

// Read submits an aysnc read requests to be notified via WaitIO()
//
// 'buf' can be set to nil to use internal buffer.
// The sequence of notification can guarantee the buffer will not be overwritten
// before next WaitIO returns
func (w *Watcher) Read(ctx interface{}, fd int, buf []byte) error {
	return w.ReadTimeout(ctx, fd, buf, time.Time{})
}

// ReadTimeout like above, submits an aysnc Read requests with timeout to be notified via WaitIO()
func (w *Watcher) ReadTimeout(ctx interface{}, fd int, buf []byte, deadline time.Time) error {
	select {
	case <-w.die:
		return ErrWatcherClosed
	default:
		w.pendingMutex.Lock()
		w.pendingReaders[fd] = append(w.pendingReaders[fd], &aiocb{ctx: ctx, fd: fd, buffer: buf, deadline: deadline})
		w.pendingMutex.Unlock()

		w.notifyPending()
		return nil
	}
}

// Write submits a write requests to be notifed via WaitIO()
//
// the notification order of Write is guaranteed to be sequential.
func (w *Watcher) Write(ctx interface{}, fd int, buf []byte) error {
	return w.WriteTimeout(ctx, fd, buf, time.Time{})
}

func (w *Watcher) WriteTimeout(ctx interface{}, fd int, buf []byte, deadline time.Time) error {
	select {
	case <-w.die:
		return ErrWatcherClosed
	default:
		w.pendingMutex.Lock()
		w.pendingWriters[fd] = append(w.pendingWriters[fd], &aiocb{ctx: ctx, fd: fd, buffer: buf, deadline: deadline})
		w.pendingMutex.Unlock()

		w.notifyPending()
		return nil
	}
}

// tryRead will try to read data on aiocb and notify
// returns true if IO has completed, false means not.
func (w *Watcher) tryRead(pcb *aiocb) (complete bool) {
	buf := pcb.buffer
	if buf == nil { // internal buffer
		buf = <-w.swapBuffer
		defer func() { w.swapBuffer <- buf }()
	}

	nr, er := syscall.Read(pcb.fd, buf)
	if er == syscall.EAGAIN {
		return false
	}
	select {
	case w.chIOCompletion <- OpResult{Op: OpRead, Fd: pcb.fd, Buffer: buf, Size: nr, Err: er, Context: pcb.ctx}:
		return true
	case <-w.die:
		return false
	}
}

func (w *Watcher) tryWrite(pcb *aiocb) (complete bool) {
	var nw int
	var ew error

	if pcb.buffer != nil {
		nw, ew = syscall.Write(pcb.fd, pcb.buffer[pcb.size:])
		if ew == syscall.EAGAIN {
			return false
		}

		// if ew is nil, accumulate bytes written
		if ew == nil {
			pcb.size += nw
		}
	}

	// all bytes written or has error
	if pcb.size == len(pcb.buffer) || ew != nil {
		select {
		case w.chIOCompletion <- OpResult{Op: OpWrite, Fd: pcb.fd, Buffer: pcb.buffer, Size: nw, Err: ew, Context: pcb.ctx}:
			return true
		case <-w.die:
			return false
		}
	}
	return false
}

// the core event loop of this watcher
func (w *Watcher) loop() {
	queuedReaders := make(map[int]*list.List)
	queuedWriters := make(map[int]*list.List)

	// nonblocking queue ops on fd
	tryReadAll := func(fd int) {
		if l, ok := queuedReaders[fd]; ok {
			for l.Len() > 0 {
				elem := l.Front()
				if w.tryRead(elem.Value.(*aiocb)) {
					l.Remove(elem)
				} else {
					return
				}
			}
		}
	}

	tryWriteAll := func(fd int) {
		if l, ok := queuedWriters[fd]; ok {
			for l.Len() > 0 {
				elem := l.Front()
				if w.tryWrite(elem.Value.(*aiocb)) {
					l.Remove(elem)
				} else {
					return
				}
			}
		}
	}

	for {
		select {
		case <-w.chPendingNotify:
			w.pendingMutex.Lock()
			for fd, cbs := range w.pendingReaders {
				l, ok := queuedReaders[fd]
				if !ok {
					l = list.New()
					queuedReaders[fd] = l
				}
				for i := range cbs {
					l.PushBack(cbs[i])
				}
				delete(w.pendingReaders, fd)
				// NOTE: API WaitIO prevents cross-deadlock on chan and mutex from happening.
				// then we can tryReadAll() to complete IO.
				tryReadAll(fd)
			}
			for fd, cbs := range w.pendingWriters {
				l, ok := queuedWriters[fd]
				if !ok {
					l = list.New()
					queuedWriters[fd] = l
				}
				for i := range cbs {
					l.PushBack(cbs[i])
				}
				delete(w.pendingWriters, fd)
				tryWriteAll(fd)
			}
			w.pendingMutex.Unlock()
		case fd := <-w.chReadableNotify:
			tryReadAll(fd)
		case fd := <-w.chWritableNotify:
			tryWriteAll(fd)
		case fd := <-w.chStopWatchNotify:
			delete(queuedReaders, fd)
			delete(queuedWriters, fd)
		case <-w.die:
			return
		}
	}
}
