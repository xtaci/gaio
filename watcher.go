// Package gaio is an Async-IO library for Golang.
//
// gaio acts in proactor mode, https://en.wikipedia.org/wiki/Proactor_pattern.
// User submit async IO operations and waits for IO-completion signal.
package gaio

import (
	"errors"
	"net"
	"reflect"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/xtaci/gaio/internal"
)

var (
	// ErrUnsupported means the watcher cannot support this type of connection
	ErrUnsupported = errors.New("unsupported connection, must be pointer")
	// ErrNoRawConn means the connection has not implemented SyscallConn
	ErrNoRawConn = errors.New("net.Conn does implement net.RawConn")
	// ErrWatcherClosed means the watcher is closed
	ErrWatcherClosed = errors.New("watcher closed")
	// ErrConnClosed means the user called Free() on related connection
	ErrConnClosed = errors.New("connection closed")
	// ErrDeadline means the specific operation has exceeded deadline before completion
	ErrDeadline = errors.New("operation exceeded deadline")
)

const (
	defaultInternalBufferSize = 65536
)

// library default watcher API
var defaultWatcher *Watcher

func init() {
	w, err := NewWatcher(defaultInternalBufferSize)
	if err != nil {
		panic(err)
	}
	defaultWatcher = w
}

// WaitIO blocks until any read/write completion, or error
func WaitIO() (r OpResult, err error) {
	return defaultWatcher.WaitIO()
}

// Read submits an async read request on 'fd' with context 'ctx', using buffer 'buf'
func Read(ctx interface{}, conn net.Conn, buf []byte) error {
	return defaultWatcher.Read(ctx, conn, buf)
}

// ReadTimeout submits an async read request on 'fd' with context 'ctx', using buffer 'buf', and
// expected to be completed before 'deadline'
func ReadTimeout(ctx interface{}, conn net.Conn, buf []byte, deadline time.Time) error {
	return defaultWatcher.ReadTimeout(ctx, conn, buf, deadline)
}

// Write submits an async write request on 'fd' with context 'ctx', using buffer 'buf'
func Write(ctx interface{}, conn net.Conn, buf []byte) error {
	return defaultWatcher.Write(ctx, conn, buf)
}

// WriteTimeout submits an async write request on 'fd' with context 'ctx', using buffer 'buf', and
// expected to be completed before 'deadline'
func WriteTimeout(ctx interface{}, conn net.Conn, buf []byte, deadline time.Time) error {
	return defaultWatcher.WriteTimeout(ctx, conn, buf, deadline)
}

// Free let the watcher to release resources related to this conn immediately, like file descriptors
func Free(conn net.Conn) error {
	return defaultWatcher.Free(conn)
}

// OpType defines Operation Type
type OpType int

const (
	// OpRead means the aiocb is a read operation
	OpRead OpType = iota
	// OpWrite means the aiocb is a write operation
	OpWrite
	// internal operation to delete an related resource
	opDelete
)

// aiocb contains all info for a request
type aiocb struct {
	ctx          interface{} // user context associated with this request
	op           OpType      // read or write
	fd           int         // associated file descriptor for nonblocking-io
	conn         net.Conn    // associated connection for nonblocking-io
	err          error       // error for last operation
	size         int         // size received or sent
	buffer       []byte
	hasCompleted bool // mark this aiocb has completed
	deadline     time.Time
}

// OpResult is the result of an aysnc-io
type OpResult struct {
	// Operation Type
	Operation OpType
	// User context associated with this requests
	Context interface{}
	// Related net.Conn to this result
	Conn net.Conn
	// Buffer points to user's supplied buffer or watcher's internal swap buffer
	Buffer []byte
	// Number of bytes sent or received, Buffer[:Size] is the content sent or received.
	Size int
	// IO error,timeout error
	Error error
}

// Watcher will monitor events and process async-io request(s),
type Watcher struct {
	// poll fd
	pfd *poller

	// netpoll events
	chEventNotify chan pollerEvents

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
	w.chEventNotify = make(chan pollerEvents)
	w.chPendingNotify = make(chan struct{}, 1)
	w.chIOCompletion = make(chan OpResult)
	w.die = make(chan struct{})

	// finalizer for system resources
	runtime.SetFinalizer(w, func(w *Watcher) {
		w.Close()
	})

	go w.pfd.Wait(w.chEventNotify, w.die)
	go w.loop()
	return w, nil
}

// Close stops monitoring on events for all connections
func (w *Watcher) Close() (err error) {
	w.dieOnce.Do(func() {
		close(w.die)
		err = w.pfd.Close()
	})
	runtime.SetFinalizer(w, nil)
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

// Free let the watcher to release resources related to this conn immediately, like file descriptors
func (w *Watcher) Free(conn net.Conn) error {
	return w.aioCreate(nil, opDelete, conn, nil, time.Time{})
}

// core async-io creation
func (w *Watcher) aioCreate(ctx interface{}, op OpType, conn net.Conn, buf []byte, deadline time.Time) error {
	select {
	case <-w.die:
		return ErrWatcherClosed
	default:
		w.pendingMutex.Lock()
		w.pending = append(w.pending, &aiocb{op: op, ctx: ctx, conn: conn, buffer: buf, deadline: deadline})
		w.pendingMutex.Unlock()

		w.notifyPending()
		return nil
	}
}

// tryRead will try to read data on aiocb and notify
func (w *Watcher) tryRead(pcb *aiocb) {
	if pcb.hasCompleted {
		return
	}

	buf := pcb.buffer
	var useSwap bool
	if buf == nil { // internal buffer
		buf = w.swapBuffer[w.nextSwapBuffer]
		useSwap = true
	}

	for {
		// return values are stored in pcb
		pcb.size, pcb.err = syscall.Read(pcb.fd, buf)
		if pcb.err == syscall.EAGAIN {
			return
		}

		// On MacOS we can see EINTR here if the user
		// pressed ^Z.
		if pcb.err == syscall.EINTR {
			continue
		}
		break
	}

	select {
	case w.chIOCompletion <- OpResult{Operation: OpRead, Conn: pcb.conn, Buffer: buf, Size: pcb.size, Error: pcb.err, Context: pcb.ctx}:
		// swap buffer if IO successful
		if useSwap {
			w.nextSwapBuffer = (w.nextSwapBuffer + 1) % len(w.swapBuffer)
		}
		pcb.hasCompleted = true
		return
	case <-w.die:
		return
	}
}

func (w *Watcher) tryWrite(pcb *aiocb) {
	var nw int
	var ew error

	if pcb.hasCompleted {
		return
	}

	if pcb.buffer != nil {
		nw, ew = syscall.Write(pcb.fd, pcb.buffer[pcb.size:])
		pcb.err = ew
		if ew == syscall.EAGAIN {
			return
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
		case w.chIOCompletion <- OpResult{Operation: OpWrite, Conn: pcb.conn, Buffer: pcb.buffer, Size: nw, Error: ew, Context: pcb.ctx}:
			pcb.hasCompleted = true
			return
		case <-w.die:
			return
		}
	}
}

// the core event loop of this watcher
func (w *Watcher) loop() {
	// the maps below is consistent at any give time.
	queuedReaders := make(map[int][]*aiocb) // ident -> aiocb
	queuedWriters := make(map[int][]*aiocb)
	connIdents := make(map[uintptr]int)
	idents := make(map[int]uintptr) // fd->net.conn
	gc := make(chan uintptr)

	// for timeout operations
	chTimeoutOps := make(chan *aiocb)

	releaseConn := func(ident int) {
		//log.Println("release", ident)
		if ptr, ok := idents[ident]; ok {
			delete(queuedReaders, ident)
			delete(queuedWriters, ident)
			delete(idents, ident)
			delete(connIdents, ptr)
			// close file descriptor
			syscall.Close(ident)
		}
	}

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
				var ptr uintptr
				if reflect.TypeOf(pcb.conn).Kind() == reflect.Ptr {
					ptr = reflect.ValueOf(pcb.conn).Pointer()
				} else {
					select {
					case w.chIOCompletion <- OpResult{Operation: pcb.op, Conn: pcb.conn, Buffer: pcb.buffer, Size: 0, Error: ErrUnsupported, Context: pcb.ctx}:
						continue
					case <-w.die:
						return
					}
				}

				ident, ok := connIdents[ptr]
				// resource release
				if pcb.op == opDelete {
					if ok {
						// for each request, send release signal
						// before resource releases.
						for _, pcb := range queuedReaders[ident] {
							select {
							case w.chIOCompletion <- OpResult{Operation: pcb.op, Conn: pcb.conn, Buffer: pcb.buffer, Size: pcb.size, Error: ErrConnClosed, Context: pcb.ctx}:
								continue
							case <-w.die:
								return
							}
						}
						for _, pcb := range queuedWriters[ident] {
							select {
							case w.chIOCompletion <- OpResult{Operation: pcb.op, Conn: pcb.conn, Buffer: pcb.buffer, Size: pcb.size, Error: ErrConnClosed, Context: pcb.ctx}:
								continue
							case <-w.die:
								return
							}
						}
						releaseConn(ident)
					}
					continue
				}

				// new conn
				if !ok {
					if dupfd, err := dupconn(pcb.conn); err != nil {
						select {
						case w.chIOCompletion <- OpResult{Operation: pcb.op, Conn: pcb.conn, Buffer: pcb.buffer, Size: 0, Error: err, Context: pcb.ctx}:
							continue
						case <-w.die:
							return
						}
					} else {
						// assign idents
						ident = dupfd

						// unexpected situation, should notify caller
						werr := w.pfd.Watch(ident)
						if werr != nil {
							select {
							case w.chIOCompletion <- OpResult{Operation: pcb.op, Conn: pcb.conn, Buffer: pcb.buffer, Size: 0, Error: werr, Context: pcb.ctx}:
								continue
							case <-w.die:
								return
							}
						}

						// bindings
						idents[ident] = ptr
						connIdents[ptr] = ident
						// as we duplicated succesfuly, we're safe to
						// close the original connection
						pcb.conn.Close()

						// the conn is still useful for GC finalizer
						runtime.SetFinalizer(pcb.conn, func(c net.Conn) {
							select {
							case gc <- reflect.ValueOf(c).Pointer():
							case <-w.die:
								return
							}
						})
					}
				}
				//log.Println("idents:", ident, ptr)
				pcb.fd = ident

				// operations splitted into different buckets
				switch pcb.op {
				case OpRead:
					if len(queuedReaders[ident]) == 0 {
						w.tryRead(pcb)
						if pcb.hasCompleted {
							if pcb.err != nil || (pcb.size == 0 && pcb.err == nil) {
								releaseConn(ident)
							}
							continue
						}
					}
					queuedReaders[ident] = append(queuedReaders[ident], pcb)
				case OpWrite:
					if len(queuedWriters[ident]) == 0 {
						w.tryWrite(pcb)
						if pcb.hasCompleted {
							if pcb.err != nil {
								releaseConn(ident)
							}
							continue
						}
					}
					queuedWriters[ident] = append(queuedWriters[ident], pcb)
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
		case pe := <-w.chEventNotify:
			// suppose fd(s) being polled is closed by conn.Close() from outside after chanrecv,
			// and a new conn is re-opened with the same handler number(fd).
			//
			// Note poller will remove closed fd automatically epoll(7), kqueue(2) and silently,
			// watcher will dup() a new fd related to net.Conn, which uniquely identified by 'e.ident'
			// then the following IO operation is impossible to misread or miswrite on
			// the new same socket fd number as in net.Conn.
			//log.Println(e)
			for _, e := range pe.events {
				if e.r {
					count := 0
					closed := false
					for _, pcb := range queuedReaders[e.ident] {
						w.tryRead(pcb)
						if pcb.hasCompleted {
							count++
							if pcb.err != nil || (pcb.size == 0 && pcb.err == nil) {
								releaseConn(e.ident)
								closed = true
								break
							}
						} else {
							break
						}
					}
					if !closed {
						queuedReaders[e.ident] = queuedReaders[e.ident][count:]
					}
				}
				if e.w {
					count := 0
					closed := false
					for _, pcb := range queuedWriters[e.ident] {
						w.tryWrite(pcb)
						if pcb.hasCompleted {
							count++
							if pcb.err != nil {
								releaseConn(e.ident)
								closed = true
								break
							}
						} else {
							break
						}
					}

					if !closed {
						queuedWriters[e.ident] = queuedWriters[e.ident][count:]
					}
				}
			}
			close(pe.done)
		case pcb := <-chTimeoutOps:
			if !pcb.hasCompleted {
				// ErrDeadline
				select {
				case w.chIOCompletion <- OpResult{Operation: pcb.op, Conn: pcb.conn, Buffer: pcb.buffer, Size: pcb.size, Error: ErrDeadline, Context: pcb.ctx}:
					pcb.hasCompleted = true
				case <-w.die:
					return
				}
			}
		case ptr := <-gc: // gc recycled net.Conn
			if ident, ok := connIdents[ptr]; ok {
				// since it's gc-ed, queue is impossible to hold net.Conn
				// just release here
				releaseConn(ident)
			}
		case <-w.die:
			return
		}

	}
}
