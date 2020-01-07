// Package gaio is an Async-IO library for Golang.
//
// gaio acts in proactor mode, https://en.wikipedia.org/wiki/Proactor_pattern.
// User submit async IO operations and waits for IO-completion signal.
package gaio

import (
	"errors"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/xtaci/gaio/internal"
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

// userspace tracking of fd status
const (
	canRead  byte = 1
	canWrite      = 2
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
	// User context associated with this requests
	Context interface{}
	// Related file descriptor to this result
	Fd int
	// Buffer points to user's supplied buffer or watcher's internal swap buffer
	Buffer []byte
	// Number of bytes sent or received, Buffer[:Size] is the content sent or received.
	Size int
	// IO error,timeout error
	Err error
}

// Watcher will monitor events and process async-io request(s),
type Watcher struct {
	// poll fd
	pfd *poller

	// loop
	chReadableNotify  chan int
	chWritableNotify  chan int
	chStopWatchNotify chan int
	chPendingNotify   chan struct{}
	chIOCompletion    chan OpResult

	// lock for pending io operations
	// aiocb is associated to fd
	pending      []*aiocb
	pendingMutex sync.Mutex

	// internal buffer for reading
	swapBuffer     [][]byte
	nextSwapBuffer int

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
	w.swapBuffer = make([][]byte, 2)
	for i := 0; i < len(w.swapBuffer); i++ {
		w.swapBuffer[i] = make([]byte, bufsize)
	}

	// loop related chan
	w.chReadableNotify = make(chan int)
	w.chWritableNotify = make(chan int)
	w.chStopWatchNotify = make(chan int)
	w.chPendingNotify = make(chan struct{}, 1)
	w.chIOCompletion = make(chan OpResult)

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
		w.connsMutex.Lock()
		for k := range w.conns {
			w.conns[k].Close()
		}
		w.connsMutex.Unlock()
	})
	return err
}

// NewConn starts monitoring events on `conn`, and returns a file descriptor
// for following IO operations.
func (w *Watcher) NewConn(conn net.Conn) (fd int, err error) {
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

// CloseConn stops event monitoring on fd, and close the related net.Conn
func (w *Watcher) CloseConn(fd int) error {
	_ = w.pfd.Unwatch(fd)
	// delete reference
	w.connsMutex.Lock()
	if conn, ok := w.conns[fd]; ok {
		conn.Close()
		delete(w.conns, fd)
	}
	w.connsMutex.Unlock()

	// notify eventloop
	select {
	case w.chStopWatchNotify <- fd:
		return nil
	case <-w.die:
		return ErrWatcherClosed
	}
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

// ReadInternal submits an async read request on 'fd' with context 'ctx', reusing internal buffer
func (w *Watcher) ReadInternal(ctx interface{}, fd int) error {
	return w.aioCreate(ctx, OpRead, fd, nil, time.Time{})
}

// Read submits an async read request on 'fd' with context 'ctx', using buffer 'buf'
func (w *Watcher) Read(ctx interface{}, fd int, buf []byte) error {
	return w.aioCreate(ctx, OpRead, fd, buf, time.Time{})
}

// ReadTimeout submits an async read request on 'fd' with context 'ctx', using buffer 'buf', and
// expected to be completed before 'deadline'
func (w *Watcher) ReadTimeout(ctx interface{}, fd int, buf []byte, deadline time.Time) error {
	return w.aioCreate(ctx, OpRead, fd, buf, deadline)
}

// Write submits an async write request on 'fd' with context 'ctx', using buffer 'buf'
func (w *Watcher) Write(ctx interface{}, fd int, buf []byte) error {
	return w.aioCreate(ctx, OpWrite, fd, buf, time.Time{})
}

// WriteTimeout submits an async write request on 'fd' with context 'ctx', using buffer 'buf', and
// expected to be completed before 'deadline'
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
		w.pending = append(w.pending, &aiocb{op: op, ctx: ctx, fd: fd, buffer: buf, deadline: deadline})
		w.pendingMutex.Unlock()

		w.notifyPending()
		return nil
	}
}

// tryRead will try to read data on aiocb and notify
// returns true if IO has completed, false means not.
func (w *Watcher) tryRead(pcb *aiocb) (complete bool) {
	if pcb.hasCompleted {
		return true
	}

	buf := pcb.buffer
	var useSwap bool
	if buf == nil { // internal buffer
		buf = w.swapBuffer[w.nextSwapBuffer]
		useSwap = true
	}

	nr, er := syscall.Read(pcb.fd, buf)
	if er == syscall.EAGAIN {
		return false
	}
	select {
	case w.chIOCompletion <- OpResult{Op: OpRead, Fd: pcb.fd, Buffer: buf, Size: nr, Err: er, Context: pcb.ctx}:
		// swap buffer if IO successful
		if useSwap {
			w.nextSwapBuffer = (w.nextSwapBuffer + 1) % len(w.swapBuffer)
		}
		pcb.hasCompleted = true
		return true
	case <-w.die:
		return false
	}
}

func (w *Watcher) tryWrite(pcb *aiocb) (complete bool) {
	var nw int
	var ew error

	if pcb.hasCompleted {
		return true
	}

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
	// nil buffer still returns
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

func (w *Watcher) tryReadAll(list []*aiocb) int {
	count := 0
	for _, pcb := range list {
		if w.tryRead(pcb) {
			count++
		} else {
			return count
		}
	}
	return count
}

func (w *Watcher) tryWriteAll(list []*aiocb) int {
	count := 0
	for _, pcb := range list {
		if w.tryWrite(pcb) {
			count++
		} else {
			return count
		}
	}
	return count
}

// the core event loop of this watcher
func (w *Watcher) loop() {
	queuedReaders := make(map[int][]*aiocb)
	queuedWriters := make(map[int][]*aiocb)
	chTimeouts := make(chan *aiocb)

	// track file descriptor status
	fdstatus := make(map[int]byte)

	// for copying
	var pending []*aiocb

	for {
		select {
		case <-w.chPendingNotify:
			// copy from w.pending to local pending
			w.pendingMutex.Lock()
			if cap(pending) < cap(w.pending) {
				pending = make([]*aiocb, 0, cap(w.pending))
			}
			pending = pending[:len(w.pending)]
			copy(pending, w.pending)
			w.pending = w.pending[:0]
			w.pendingMutex.Unlock()

			for _, pcb := range pending {
				// load status of fd
				status, ok := fdstatus[pcb.fd]
				if !ok { // new fd, initial status set
					status = canRead | canWrite
					fdstatus[pcb.fd] = status
				}

				switch pcb.op {
				case OpRead:
					if len(queuedReaders[pcb.fd]) == 0 && status&canRead != 0 {
						// empty queue should try IO first
						if w.tryRead(pcb) {
							continue
						} else {
							status &^= canRead
							fdstatus[pcb.fd] = status
						}
					}
					queuedReaders[pcb.fd] = append(queuedReaders[pcb.fd], pcb)
				case OpWrite:
					if len(queuedWriters[pcb.fd]) == 0 && status&canWrite != 0 {
						if w.tryWrite(pcb) {
							continue
						} else {
							status &^= canWrite
							fdstatus[pcb.fd] = status
						}

					}
					queuedWriters[pcb.fd] = append(queuedWriters[pcb.fd], pcb)
				}

				// timer
				if !pcb.deadline.IsZero() {
					timedpcb := pcb
					internal.SystemTimedSched.Put(func() {
						chTimeouts <- timedpcb
					}, timedpcb.deadline)
				}
			}
			pending = pending[:0]
		case fd := <-w.chReadableNotify:
			n := w.tryReadAll(queuedReaders[fd])
			if n == len(queuedReaders[fd]) { // all read complete, or n==0
				fdstatus[fd] = fdstatus[fd] | canRead
			}
			queuedReaders[fd] = queuedReaders[fd][n:]
		case fd := <-w.chWritableNotify:
			n := w.tryWriteAll(queuedWriters[fd])
			if n == len(queuedWriters[fd]) {
				fdstatus[fd] = fdstatus[fd] | canWrite
			}
			queuedWriters[fd] = queuedWriters[fd][n:]
		case fd := <-w.chStopWatchNotify:
			delete(queuedReaders, fd)
			delete(queuedWriters, fd)
			delete(fdstatus, fd)
		case pcb := <-chTimeouts:
			if !pcb.hasCompleted {
				// ErrDeadline
				select {
				case w.chIOCompletion <- OpResult{Op: pcb.op, Fd: pcb.fd, Buffer: pcb.buffer, Size: pcb.size, Err: ErrDeadline, Context: pcb.ctx}:
					pcb.hasCompleted = true
				case <-w.die:
					return
				}
			}
		case <-w.die:
			return
		}
	}
}
