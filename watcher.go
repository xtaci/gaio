// Package gaio is an Async-IO library for Golang.
//
// gaio acts in proactor mode, https://en.wikipedia.org/wiki/Proactor_pattern.
// User submit async IO operations and waits for IO-completion signal.
package gaio

import (
	"container/heap"
	"container/list"
	"errors"
	"io"
	"net"
	"reflect"
	"runtime"
	"sync"
	"syscall"
	"time"
)

var (
	// ErrUnsupported means the watcher cannot support this type of connection
	ErrUnsupported = errors.New("unsupported connection, must be pointer")
	// ErrNoRawConn means the connection has not implemented SyscallConn
	ErrNoRawConn = errors.New("net.Conn does implement net.RawConn")
	// ErrWatcherClosed means the watcher is closed
	ErrWatcherClosed = errors.New("watcher closed")
	// ErrPollerClosed suggest that poller has closed
	ErrPollerClosed = errors.New("poller closed")
	// ErrConnClosed means the user called Free() on related connection
	ErrConnClosed = errors.New("connection closed")
	// ErrDeadline means the specific operation has exceeded deadline before completion
	ErrDeadline = errors.New("operation exceeded deadline")
	// ErrEmptyBuffer means the buffer is nil
	ErrEmptyBuffer = errors.New("empty buffer")
)

var (
	zeroTime = time.Time{}
)

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

// aiocb contains all info for a single request
type aiocb struct {
	l            *list.List // list where this request belongs to
	elem         *list.Element
	ctx          interface{} // user context associated with this request
	ptr          uintptr     // pointer to conn
	op           OpType      // read or write
	conn         net.Conn    // associated connection for nonblocking-io
	err          error       // error for last operation
	size         int         // size received or sent
	buffer       []byte
	useSwap      bool // mark if the buffer is internal swap buffer
	notifyCaller bool // mark if the caller have to wakeup caller to swap buffer.
	idx          int  // index for heap op
	deadline     time.Time
}

// fdDesc contains all data structures associated to fd
type fdDesc struct {
	readers list.List // all read/write requests
	writers list.List
	ptr     uintptr // pointer to net.Conn
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
	// IsSwapBuffer marks true if the buffer internal one
	IsSwapBuffer bool
	// Number of bytes sent or received, Buffer[:Size] is the content sent or received.
	Size int
	// IO error,timeout error
	Error error
}

// Watcher will monitor events and process async-io request(s),
type Watcher struct {
	// a wrapper for watcher for gc purpose
	*watcher
}

// watcher will monitor events and process async-io request(s),
type watcher struct {
	// poll fd
	pfd *poller

	// netpoll events
	chEventNotify chan pollerEvents

	// events from user
	chPendingNotify chan struct{}

	// IO-completion events to user
	chNotifyCompletion chan []OpResult

	// lock for pending io operations
	// aiocb is associated to fd
	pending      []*aiocb
	pendingMutex sync.Mutex

	// internal buffer for reading
	swapSize       int // swap buffer capacity
	swapBuffer     [][]byte
	bufferOffset   int // bufferOffset for current using one
	nextSwapBuffer int

	// loop related data structure
	descs      map[int]*fdDesc // all descriptors
	connIdents map[uintptr]int // we must not hold net.Conn as key, for GC purpose
	// for timeout operations which
	// aiocb has non-zero deadline, either exists
	// in timeouts & queue at any time
	// or in neither of them.
	timeouts timedHeap
	timer    *time.Timer
	// for garbage collector
	gc       []net.Conn
	gcMutex  sync.Mutex
	gcNotify chan struct{}

	die     chan struct{}
	dieOnce sync.Once
}

const (
	defaultInternalBufferSize = 65536
)

// NewWatcher creates a management object for monitoring file descriptors
// with default internal buffer size - 64KB
func NewWatcher() (*Watcher, error) {
	return NewWatcherSize(defaultInternalBufferSize)
}

// NewWatcherSize creates a management object for monitoring file descriptors.
// 'bufsize' sets the internal swap buffer size for Read() with nil, 2 slices with'bufsize'
// will be allocated for performance.
func NewWatcherSize(bufsize int) (*Watcher, error) {
	w := new(watcher)
	pfd, err := openPoll()
	if err != nil {
		return nil, err
	}
	w.pfd = pfd

	// loop related chan
	w.chEventNotify = make(chan pollerEvents, eventQueueSize)
	w.chPendingNotify = make(chan struct{}, 1)
	w.chNotifyCompletion = make(chan []OpResult)
	w.die = make(chan struct{})

	// swapBuffer for shared reading
	w.swapSize = bufsize
	w.swapBuffer = make([][]byte, 2)
	for i := 0; i < len(w.swapBuffer); i++ {
		w.swapBuffer[i] = make([]byte, bufsize)
	}

	// init loop related data structures
	w.descs = make(map[int]*fdDesc)
	w.connIdents = make(map[uintptr]int)
	w.gcNotify = make(chan struct{}, 1)
	w.timer = time.NewTimer(0)

	go w.pfd.Wait(w.chEventNotify)
	go w.loop()

	// watcher finalizer for system resources
	wrapper := &Watcher{watcher: w}
	runtime.SetFinalizer(wrapper, func(wrapper *Watcher) {
		wrapper.Close()
	})

	return wrapper, nil
}

// Close stops monitoring on events for all connections
func (w *watcher) Close() (err error) {
	w.dieOnce.Do(func() {
		close(w.die)
		err = w.pfd.Close()
	})
	return err
}

// notify new operations pending
func (w *watcher) notifyPending() {
	select {
	case w.chPendingNotify <- struct{}{}:
	default:
	}
}

// WaitIO blocks until any read/write completion, or error.
// An internal 'buf' returned is safe to use until next WaitIO returns.
func (w *watcher) WaitIO() (r []OpResult, err error) {
	select {
	case r := <-w.chNotifyCompletion:
		return r, nil
	case <-w.die:
		return r, ErrWatcherClosed
	}
}

// Read submits an async read request on 'fd' with context 'ctx', using buffer 'buf'.
// 'buf' can be set to nil to use internal buffer.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
func (w *watcher) Read(ctx interface{}, conn net.Conn, buf []byte) error {
	return w.aioCreate(ctx, OpRead, conn, buf, zeroTime)
}

// ReadTimeout submits an async read request on 'fd' with context 'ctx', using buffer 'buf', and
// expected to be completed before 'deadline'.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
func (w *watcher) ReadTimeout(ctx interface{}, conn net.Conn, buf []byte, deadline time.Time) error {
	return w.aioCreate(ctx, OpRead, conn, buf, deadline)
}

// Write submits an async write request on 'fd' with context 'ctx', using buffer 'buf'.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
func (w *watcher) Write(ctx interface{}, conn net.Conn, buf []byte) error {
	if len(buf) == 0 {
		return ErrEmptyBuffer
	}
	return w.aioCreate(ctx, OpWrite, conn, buf, zeroTime)
}

// WriteTimeout submits an async write request on 'fd' with context 'ctx', using buffer 'buf', and
// expected to be completed before 'deadline', 'buf' can be set to nil to use internal buffer.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
func (w *watcher) WriteTimeout(ctx interface{}, conn net.Conn, buf []byte, deadline time.Time) error {
	if len(buf) == 0 {
		return ErrEmptyBuffer
	}
	return w.aioCreate(ctx, OpWrite, conn, buf, deadline)
}

// Free let the watcher to release resources related to this conn immediately,
// like socket file descriptors.
func (w *watcher) Free(conn net.Conn) error {
	return w.aioCreate(nil, opDelete, conn, nil, zeroTime)
}

// core async-io creation
func (w *watcher) aioCreate(ctx interface{}, op OpType, conn net.Conn, buf []byte, deadline time.Time) error {
	select {
	case <-w.die:
		return ErrWatcherClosed
	default:
		var ptr uintptr
		if reflect.TypeOf(conn).Kind() == reflect.Ptr {
			ptr = reflect.ValueOf(conn).Pointer()
		} else {
			return ErrUnsupported
		}
		w.pendingMutex.Lock()
		w.pending = append(w.pending, &aiocb{op: op, ptr: ptr, ctx: ctx, conn: conn, buffer: buf, deadline: deadline})
		w.pendingMutex.Unlock()

		w.notifyPending()
		return nil
	}
}

// tryRead will try to read data on aiocb and notify
func (w *watcher) tryRead(fd int, pcb *aiocb) bool {
	buf := pcb.buffer

	var useSwap bool
	if buf == nil { // internal buffer
		buf = w.swapBuffer[w.nextSwapBuffer][w.bufferOffset:]
		useSwap = true
	}

	for {
		// return values are stored in pcb
		pcb.size, pcb.err = syscall.Read(fd, buf)
		if pcb.err == syscall.EAGAIN {
			return false
		}

		// On MacOS we can see EINTR here if the user
		// pressed ^Z.
		if pcb.err == syscall.EINTR {
			continue
		}

		// proper setting of EOF
		if pcb.size == 0 && pcb.err == nil {
			pcb.err = io.EOF
		}

		break
	}

	// IO completed successfully with internal buffer
	if useSwap && pcb.err == nil {
		pcb.buffer = buf[:pcb.size] // set len to pcb.size
		pcb.useSwap = true
		w.bufferOffset += pcb.size

		// current buffer exhausted, notify caller and swap buffer
		if w.bufferOffset == w.swapSize {
			w.nextSwapBuffer = (w.nextSwapBuffer + 1) % len(w.swapBuffer)
			w.bufferOffset = 0
			pcb.notifyCaller = true
		}
	}

	return true
}

func (w *watcher) tryWrite(fd int, pcb *aiocb) bool {
	var nw int
	var ew error

	if pcb.buffer != nil {
		for {
			nw, ew = syscall.Write(fd, pcb.buffer[pcb.size:])
			pcb.err = ew
			if ew == syscall.EAGAIN {
				return false
			}

			if ew == syscall.EINTR {
				continue
			}

			// if ew is nil, accumulate bytes written
			if ew == nil {
				pcb.size += nw
			}
			break
		}
	}

	// all bytes written or has error
	// nil buffer still returns
	if pcb.size == len(pcb.buffer) || ew != nil {
		return true
	}
	return false
}

// release connection related resources
func (w *watcher) releaseConn(ident int) {
	if desc, ok := w.descs[ident]; ok {
		// delete from heap
		for e := desc.readers.Front(); e != nil; e = e.Next() {
			tcb := e.Value.(*aiocb)
			if !tcb.deadline.IsZero() {
				heap.Remove(&w.timeouts, tcb.idx)
			}
		}

		for e := desc.writers.Front(); e != nil; e = e.Next() {
			tcb := e.Value.(*aiocb)
			if !tcb.deadline.IsZero() {
				heap.Remove(&w.timeouts, tcb.idx)
			}
		}

		delete(w.descs, ident)
		delete(w.connIdents, desc.ptr)
		// close socket file descriptor duplicated from net.Conn
		syscall.Close(ident)
	}
}

// the core event loop of this watcher
func (w *watcher) loop() {
	// defer function to release all resources
	defer func() {
		for ident := range w.descs {
			w.releaseConn(ident)
		}
	}()

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
			w.handlePending(pending)

		case pe := <-w.chEventNotify: // poller events
			w.handleEvents(pe)

		case <-w.timer.C: // timeout heap
			for w.timeouts.Len() > 0 {
				now := time.Now()
				pcb := w.timeouts[0]
				if now.After(pcb.deadline) {
					// remove from list & heap together
					pcb.l.Remove(pcb.elem)
					heap.Pop(&w.timeouts)

					// ErrDeadline
					select {
					case w.chNotifyCompletion <- []OpResult{{Operation: pcb.op, Conn: pcb.conn, Buffer: pcb.buffer, Size: pcb.size, Error: ErrDeadline, Context: pcb.ctx}}:
					case <-w.die:
						return
					}
				} else {
					w.timer.Reset(pcb.deadline.Sub(now))
					break
				}
			}

		case <-w.gcNotify: // gc recycled net.Conn
			w.gcMutex.Lock()
			for i, c := range w.gc {
				ptr := reflect.ValueOf(c).Pointer()
				if ident, ok := w.connIdents[ptr]; ok {
					// since it's gc-ed, queue is impossible to hold net.Conn
					// we don't have to send to chIOCompletion,just release here
					w.releaseConn(ident)
				}
				w.gc[i] = nil
			}
			w.gc = w.gc[:0]
			w.gcMutex.Unlock()

		case <-w.die:
			return
		}
	}
}

// for loop handling pending requests
func (w *watcher) handlePending(pending []*aiocb) {
	for _, pcb := range pending {
		ident, ok := w.connIdents[pcb.ptr]
		// resource releasing operation
		if pcb.op == opDelete && ok {
			w.releaseConn(ident)
			continue
		}

		// handling new connection
		var desc *fdDesc
		if ok {
			desc = w.descs[ident]
		} else {
			if dupfd, err := dupconn(pcb.conn); err != nil {
				select {
				case w.chNotifyCompletion <- []OpResult{{Operation: pcb.op, Conn: pcb.conn, Buffer: pcb.buffer, Size: 0, Error: err, Context: pcb.ctx}}:
				case <-w.die:
					return
				}
				continue
			} else {
				// as we duplicated successfully, we're safe to
				// close the original connection
				pcb.conn.Close()
				// assign idents
				ident = dupfd

				// unexpected situation, should notify caller if we cannot dup(2)
				werr := w.pfd.Watch(ident)
				if werr != nil {
					select {
					case w.chNotifyCompletion <- []OpResult{{Operation: pcb.op, Conn: pcb.conn, Buffer: pcb.buffer, Size: 0, Error: werr, Context: pcb.ctx}}:
					case <-w.die:
						return
					}
					continue
				}

				// file description bindings
				desc = &fdDesc{ptr: pcb.ptr}
				w.descs[ident] = desc
				w.connIdents[pcb.ptr] = ident

				// the conn is still useful for GC finalizer.
				// note finalizer function cannot hold reference to net.Conn,
				// if not it will never be GC-ed.
				runtime.SetFinalizer(pcb.conn, func(c net.Conn) {
					w.gcMutex.Lock()
					w.gc = append(w.gc, c)
					w.gcMutex.Unlock()

					// notify gc processor
					select {
					case w.gcNotify <- struct{}{}:
					default:
					}
				})
			}
		}

		// operations splitted into different buckets
		switch pcb.op {
		case OpRead:
			// try immediately queue is empty
			if desc.readers.Len() == 0 {
				if w.tryRead(ident, pcb) {
					select {
					case w.chNotifyCompletion <- []OpResult{{Operation: OpRead, Conn: pcb.conn, IsSwapBuffer: pcb.useSwap, Buffer: pcb.buffer, Size: pcb.size, Error: pcb.err, Context: pcb.ctx}}:
					case <-w.die:
						return
					}
					continue
				}
			}
			// enqueue for poller events
			pcb.l = &desc.readers
			pcb.elem = pcb.l.PushBack(pcb)
		case OpWrite:
			if desc.writers.Len() == 0 {
				if w.tryWrite(ident, pcb) {
					select {
					case w.chNotifyCompletion <- []OpResult{{Operation: OpWrite, Conn: pcb.conn, Buffer: pcb.buffer, Size: pcb.size, Error: pcb.err, Context: pcb.ctx}}:
					case <-w.die:
						return
					}
					continue
				}
			}
			pcb.l = &desc.writers
			pcb.elem = pcb.l.PushBack(pcb)
		}

		// push to heap for timeout operation
		if !pcb.deadline.IsZero() {
			heap.Push(&w.timeouts, pcb)
			if w.timeouts.Len() == 1 {
				w.timer.Reset(pcb.deadline.Sub(time.Now()))
			}
		}
	}
}

// handle poller events
func (w *watcher) handleEvents(pe pollerEvents) {
	// suppose fd(s) being polled is closed by conn.Close() from outside after chanrecv,
	// and a new conn has re-opened with the same handler number(fd). The read and write
	// on this fd is fatal.
	//
	// Note poller will remove closed fd automatically epoll(7), kqueue(2) and silently.
	// To solve this problem watcher will dup() a new fd from net.Conn, which uniquely
	// identified by 'e.ident', all library operation will be based on 'e.ident',
	// then IO operation is impossible to misread or miswrite on re-created fd.
	//log.Println(e)
	var results []OpResult
	for _, e := range pe {
		if desc, ok := w.descs[e.ident]; ok {
			if e.r {
				var next *list.Element
				for elem := desc.readers.Front(); elem != nil; elem = next {
					next = elem.Next()
					pcb := elem.Value.(*aiocb)
					if w.tryRead(e.ident, pcb) {
						results = append(results, OpResult{Operation: OpRead, Conn: pcb.conn, Buffer: pcb.buffer, IsSwapBuffer: pcb.useSwap, Size: pcb.size, Error: pcb.err, Context: pcb.ctx})
						if pcb.notifyCaller {
							select {
							case w.chNotifyCompletion <- results:
								results = nil
							case <-w.die:
								return
							}
						}
						desc.readers.Remove(elem)
						if !pcb.deadline.IsZero() {
							heap.Remove(&w.timeouts, pcb.idx)
						}
					} else {
						break
					}
				}
			}

			if e.w {
				var next *list.Element
				for elem := desc.writers.Front(); elem != nil; elem = next {
					next = elem.Next()
					pcb := elem.Value.(*aiocb)
					if w.tryWrite(e.ident, pcb) {
						results = append(results, OpResult{Operation: OpWrite, Conn: pcb.conn, Buffer: pcb.buffer, Size: pcb.size, Error: pcb.err, Context: pcb.ctx})
						desc.writers.Remove(elem)
						if !pcb.deadline.IsZero() {
							heap.Remove(&w.timeouts, pcb.idx)
						}
					} else {
						break
					}
				}
			}
		}
	}

	// batch notification
	if len(results) > 0 {
		select {
		case w.chNotifyCompletion <- results:
		case <-w.die:
			return
		}
	}
}
