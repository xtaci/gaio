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
	// ErrNoRawConn means the connection has not implemented SyscallConn
	ErrNoRawConn = errors.New("net.Conn does implement net.RawConn")
	// ErrWatcherClosed means the watcher is closed
	ErrWatcherClosed = errors.New("watcher closed")
	// ErrDeadline means the specific operation has exceeded deadline before completion
	ErrDeadline = errors.New("operation exceeded deadline")
)

// OpType defines Operation Type
type OpType int

const (
	// OpRead means the aiocb is a read operation
	OpRead OpType = iota
	// OpWrite means the aiocb is a write operation
	OpWrite
)

// aiocb contains all info for a request
type aiocb struct {
	ctx          interface{} // user context associated with this request
	op           OpType      // read or write
	fd           int
	size         int // size received or sent
	buffer       []byte
	hasCompleted bool // mark this aiocb has completed
	deadline     time.Time
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
	pending      map[int][]*aiocb
	pendingMutex sync.Mutex

	// internal buffer for reading
	swapBuffer chan []byte

	die     chan struct{}
	dieOnce sync.Once

	// hold net.Conn to avoid GC
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
	w.pending = make(map[int][]*aiocb)

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
	err = w.pfd.Watch(fd)
	if err != nil {
		return 0, err
	}

	// avoid GC of conn
	w.connsMutex.Lock()
	w.conns[fd] = conn
	w.connsMutex.Unlock()
	return fd, nil
}

// StopWatch events related to this fd
func (w *Watcher) StopWatch(fd int) error {
	err := w.pfd.Unwatch(fd)
	if err != nil {
		return err
	}

	// delete reference
	w.connsMutex.Lock()
	delete(w.conns, fd)
	w.connsMutex.Unlock()

	// notify eventloop
	select {
	case w.chStopWatchNotify <- fd:
	case <-w.die:
		return ErrWatcherClosed
	}
	return nil
}

// notify new operations pending
func (w *Watcher) notifyPending() {
	select {
	case w.chPendingNotify <- struct{}{}:
	default:
	}
}

// WaitIO blocks until any read/write completion, or error
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
	return w.aioCreate(ctx, OpRead, fd, buf, time.Time{})
}

// ReadTimeout like above, submits an aysnc Read requests with timeout to be notified via WaitIO()
func (w *Watcher) ReadTimeout(ctx interface{}, fd int, buf []byte, deadline time.Time) error {
	return w.aioCreate(ctx, OpRead, fd, buf, deadline)
}

// Write submits a write requests to be notifed via WaitIO()
//
// the notification order of Write is guaranteed to be sequential.
func (w *Watcher) Write(ctx interface{}, fd int, buf []byte) error {
	return w.aioCreate(ctx, OpWrite, fd, buf, time.Time{})
}

// WriteTimeout like above, submits an aysnc Write requests with timeout to be notified via WaitIO()
func (w *Watcher) WriteTimeout(ctx interface{}, fd int, buf []byte, deadline time.Time) error {
	return w.aioCreate(ctx, OpWrite, fd, buf, deadline)
}

// core async-io creation
func (w *Watcher) aioCreate(ctx interface{}, op OpType, fd int, buf []byte, deadline time.Time) error {
	select {
	case <-w.die:
		return ErrWatcherClosed
	default:
		w.pendingMutex.Lock()
		w.pending[fd] = append(w.pending[fd], &aiocb{op: op, ctx: ctx, fd: fd, buffer: buf, deadline: deadline})
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
		pcb.hasCompleted = true
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
			pcb.hasCompleted = true
			return true
		case <-w.die:
			return false
		}
	}
	return false
}

func (w *Watcher) tryReadAll(l *list.List) {
	for l.Len() > 0 {
		elem := l.Front()
		if w.tryRead(elem.Value.(*aiocb)) {
			l.Remove(elem)
		} else {
			return
		}
	}
}

func (w *Watcher) tryWriteAll(l *list.List) {
	for l.Len() > 0 {
		elem := l.Front()
		if w.tryWrite(elem.Value.(*aiocb)) {
			l.Remove(elem)
		} else {
			return
		}
	}
}

// the core event loop of this watcher
func (w *Watcher) loop() {
	queuedReaders := make(map[int]*list.List)
	queuedWriters := make(map[int]*list.List)
	chTimeouts := make(chan *list.Element)

	for {
		select {
		case <-w.chPendingNotify:
			w.pendingMutex.Lock()
			for fd, cbs := range w.pending {
				lr, ok := queuedReaders[fd]
				if !ok {
					lr = list.New()
					queuedReaders[fd] = lr
				}
				lw, ok := queuedWriters[fd]
				if !ok {
					lw = list.New()
					queuedWriters[fd] = lw
				}

				for i := range cbs {
					var e *list.Element
					switch cbs[i].op {
					case OpRead:
						e = lr.PushBack(cbs[i])
					case OpWrite:
						e = lw.PushBack(cbs[i])
					}

					if !cbs[i].deadline.IsZero() {
						SystemTimedSched.Put(func() {
							chTimeouts <- e
						}, cbs[i].deadline)
					}
				}
				delete(w.pending, fd)
				// NOTE: API WaitIO avoids cross-deadlock between chan and mutex
				// then we can try to complete IO here.
				w.tryReadAll(lr)
				w.tryWriteAll(lw)
			}
			w.pendingMutex.Unlock()
		case fd := <-w.chReadableNotify:
			if l, ok := queuedReaders[fd]; ok {
				w.tryReadAll(l)
			}
		case fd := <-w.chWritableNotify:
			if l, ok := queuedWriters[fd]; ok {
				w.tryWriteAll(l)
			}
		case fd := <-w.chStopWatchNotify:
			delete(queuedReaders, fd)
			delete(queuedWriters, fd)
		case e := <-chTimeouts:
			pcb := e.Value.(*aiocb)
			if !pcb.hasCompleted {
				switch pcb.op {
				case OpRead:
					if l, ok := queuedReaders[pcb.fd]; ok {
						l.Remove(e)
					}
				case OpWrite:
					if l, ok := queuedWriters[pcb.fd]; ok {
						l.Remove(e)
					}
				}

				// ErrDeadline
				select {
				case w.chIOCompletion <- OpResult{Op: pcb.op, Fd: pcb.fd, Buffer: pcb.buffer, Size: pcb.size, Err: ErrDeadline, Context: pcb.ctx}:
				case <-w.die:
					return
				}
			}
		case <-w.die:
			return
		}
	}
}
