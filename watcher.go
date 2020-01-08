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
	// ErrNotWatched means the file descriptor is not being watched
	ErrNotWatched = errors.New("file descriptor is not being watched")
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
	ctx          interface{}     // user context associated with this request
	op           OpType          // read or write
	conn         net.Conn        // associated net.Conn
	rawconn      syscall.RawConn // associated raw connection for nonblocking-io
	size         int             // size received or sent
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
	// Related net.Conn to this result
	Conn net.Conn
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

	// events from poller
	chReadableNotify chan net.Conn
	chWritableNotify chan net.Conn
	chRemovedNotify  chan net.Conn

	// events from user
	chPendingNotify chan struct{}

	// IO-completion events to user
	chIOCompletion chan OpResult

	// lock for pending io operations
	// aiocb is associated to fd
	pending      []*aiocb
	pendingMutex sync.Mutex

	// internal buffer for reading
	swapBuffer     [][]byte
	nextSwapBuffer int

	die     chan struct{}
	dieOnce sync.Once
}

// NewWatcher creates a management object for monitoring file descriptors
func NewWatcher(bufsize int) (*Watcher, error) {
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
	w.chReadableNotify = make(chan net.Conn)
	w.chWritableNotify = make(chan net.Conn)
	w.chRemovedNotify = make(chan net.Conn)
	w.chPendingNotify = make(chan struct{}, 1)
	w.chIOCompletion = make(chan OpResult)
	w.die = make(chan struct{})

	go w.pfd.Wait(w.chReadableNotify, w.chWritableNotify, w.chRemovedNotify, w.die)
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
func (w *Watcher) ReadInternal(ctx interface{}, conn net.Conn) error {
	return w.aioCreate(ctx, OpRead, conn, nil, time.Time{})
}

// Read submits an async read request on 'fd' with context 'ctx', using buffer 'buf'
func (w *Watcher) Read(ctx interface{}, conn net.Conn, buf []byte) error {
	return w.aioCreate(ctx, OpRead, conn, buf, time.Time{})
}

// ReadTimeout submits an async read request on 'fd' with context 'ctx', using buffer 'buf', and
// expected to be completed before 'deadline'
func (w *Watcher) ReadTimeout(ctx interface{}, conn net.Conn, buf []byte, deadline time.Time) error {
	return w.aioCreate(ctx, OpRead, conn, buf, deadline)
}

// Write submits an async write request on 'fd' with context 'ctx', using buffer 'buf'
func (w *Watcher) Write(ctx interface{}, conn net.Conn, buf []byte) error {
	return w.aioCreate(ctx, OpWrite, conn, buf, time.Time{})
}

// WriteTimeout submits an async write request on 'fd' with context 'ctx', using buffer 'buf', and
// expected to be completed before 'deadline'
func (w *Watcher) WriteTimeout(ctx interface{}, conn net.Conn, buf []byte, deadline time.Time) error {
	return w.aioCreate(ctx, OpWrite, conn, buf, deadline)
}

// core async-io creation
func (w *Watcher) aioCreate(ctx interface{}, op OpType, conn net.Conn, buf []byte, deadline time.Time) error {
	select {
	case <-w.die:
		return ErrWatcherClosed
	default:

		w.pendingMutex.Lock()
		w.pending = append(w.pending, &aiocb{op: op, ctx: ctx, conn: conn, rawconn: nil, buffer: buf, deadline: deadline})
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

	var nr int
	var er error
	err := pcb.rawconn.Read(func(s uintptr) bool {
		nr, er = syscall.Read(int(s), buf)
		return true
	})

	if er == syscall.EAGAIN {
		return false
	}

	// rawconn error
	if err != nil {
		er = err
	}

	select {
	case w.chIOCompletion <- OpResult{Op: OpRead, Conn: pcb.conn, Buffer: buf, Size: nr, Err: er, Context: pcb.ctx}:
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
	var err error

	if pcb.hasCompleted {
		return true
	}

	if pcb.buffer != nil {
		err = pcb.rawconn.Write(func(s uintptr) bool {
			nw, ew = syscall.Write(int(s), pcb.buffer[pcb.size:])
			return true
		})

		if ew == syscall.EAGAIN {
			return false
		}

		// rawconn error
		if err != nil {
			ew = err
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
		case w.chIOCompletion <- OpResult{Op: OpWrite, Conn: pcb.conn, Buffer: pcb.buffer, Size: nw, Err: ew, Context: pcb.ctx}:
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
	// cached raw conn
	cachedRawConns := make(map[net.Conn]syscall.RawConn)
	// net.Conn related operations
	queuedReaders := make(map[net.Conn][]*aiocb)
	queuedWriters := make(map[net.Conn][]*aiocb)
	// for timeout operations
	chTimeoutOps := make(chan *aiocb)

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
				rawconn, ok := cachedRawConns[pcb.conn]
				if !ok {
					// cache raw conn
					if c, ok := pcb.conn.(interface {
						SyscallConn() (syscall.RawConn, error)
					}); ok {
						rc, err := c.SyscallConn()
						if err == nil {
							cachedRawConns[pcb.conn] = rc
							w.pfd.Watch(pcb.conn, rc)
							rawconn = rc
						}
					}
				}

				// conns which cannot get rawconn, send back error
				if rawconn == nil {
					select {
					case w.chIOCompletion <- OpResult{Op: pcb.op, Conn: pcb.conn, Buffer: pcb.buffer, Size: pcb.size, Err: ErrNoRawConn, Context: pcb.ctx}:
						pcb.hasCompleted = true
					case <-w.die:
						return
					}
					continue
				}

				// operations splitted into different buckets
				pcb.rawconn = rawconn
				switch pcb.op {
				case OpRead:
					if len(queuedReaders[pcb.conn]) == 0 {
						if w.tryRead(pcb) {
							continue
						}
					}
					queuedReaders[pcb.conn] = append(queuedReaders[pcb.conn], pcb)
				case OpWrite:
					if len(queuedWriters[pcb.conn]) == 0 {
						if w.tryWrite(pcb) {
							continue
						}
					}
					queuedWriters[pcb.conn] = append(queuedWriters[pcb.conn], pcb)
				}

				// timer
				if !pcb.deadline.IsZero() {
					timedpcb := pcb
					internal.SystemTimedSched.Put(func() {
						select {
						case chTimeoutOps <- timedpcb:
						case <-w.die:
						}
					}, timedpcb.deadline)
				}
			}
			pending = pending[:0]
		case conn := <-w.chReadableNotify:
			// if fd(s) being polled is closed by conn.Close() from outside after chanrecv,
			// and a new conn is re-opened with the same handler number(fd).
			//
			// Note poller will removed closed fd automatically epoll(7), kqueue(2),
			// tryRead/tryWrite operates on raw connections related to net.Conn,
			// the following IO operation is impossible to misread or miswrite on
			// the new same socket fd number inside current process(rawConn.Read/Write will fail).
			n := w.tryReadAll(queuedReaders[conn])
			queuedReaders[conn] = queuedReaders[conn][n:]
			if len(queuedReaders[conn]) == 0 { // delete key from map
				delete(queuedReaders, conn)
			}
		case conn := <-w.chWritableNotify:
			n := w.tryWriteAll(queuedWriters[conn])
			queuedWriters[conn] = queuedWriters[conn][n:]
			if len(queuedWriters[conn]) == 0 {
				delete(queuedWriters, conn)
			}
		case conn := <-w.chRemovedNotify:
			// remove should also be idempotent operations
			delete(queuedWriters, conn)
			delete(queuedReaders, conn)
			delete(cachedRawConns, conn)
		case pcb := <-chTimeoutOps:
			if !pcb.hasCompleted {
				// ErrDeadline
				select {
				case w.chIOCompletion <- OpResult{Op: pcb.op, Conn: pcb.conn, Buffer: pcb.buffer, Size: pcb.size, Err: ErrDeadline, Context: pcb.ctx}:
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
