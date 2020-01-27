// Package gaio is an Async-IO library for Golang.
//
// gaio acts in proactor mode, https://en.wikipedia.org/wiki/Proactor_pattern.
// User submit async IO operations and waits for IO-completion signal.
package gaio

import (
	"container/heap"
	"container/list"
	"errors"
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
	// ErrConnClosed means the user called Free() on related connection
	ErrConnClosed = errors.New("connection closed")
	// ErrDeadline means the specific operation has exceeded deadline before completion
	ErrDeadline = errors.New("operation exceeded deadline")
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

// aiocb contains all info for a request
type aiocb struct {
	l        *list.List // list where this request belongs to
	elem     *list.Element
	ctx      interface{} // user context associated with this request
	ptr      uintptr     // pointer to conn
	op       OpType      // read or write
	fd       int         // associated file descriptor for nonblocking-io
	conn     net.Conn    // associated connection for nonblocking-io
	err      error       // error for last operation
	size     int         // size received or sent
	buffer   []byte
	idx      int // index for heap op
	deadline time.Time
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
// 'bufsize' is used to set internal swap buffer size for `Read()` API
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
		close(w.die)
		w.pfd.Close()
	})

	go w.pfd.Wait(w.chEventNotify, w.die)
	go w.loop()
	return w, nil
}

// Close stops monitoring on events for all connections
func (w *Watcher) Close() (err error) {
	runtime.SetFinalizer(w, nil)
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

// Read submits an async read request on 'fd' with context 'ctx', using buffer 'buf'.
// 'buf' can be set to nil to use internal buffer.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
func (w *Watcher) Read(ctx interface{}, conn net.Conn, buf []byte) error {
	return w.aioCreate(ctx, OpRead, conn, buf, zeroTime)
}

// ReadTimeout submits an async read request on 'fd' with context 'ctx', using buffer 'buf', and
// expected to be completed before 'deadline'.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
func (w *Watcher) ReadTimeout(ctx interface{}, conn net.Conn, buf []byte, deadline time.Time) error {
	return w.aioCreate(ctx, OpRead, conn, buf, deadline)
}

// Write submits an async write request on 'fd' with context 'ctx', using buffer 'buf'.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
func (w *Watcher) Write(ctx interface{}, conn net.Conn, buf []byte) error {
	return w.aioCreate(ctx, OpWrite, conn, buf, zeroTime)
}

// WriteTimeout submits an async write request on 'fd' with context 'ctx', using buffer 'buf', and
// expected to be completed before 'deadline', 'buf' can be set to nil to use internal buffer.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
func (w *Watcher) WriteTimeout(ctx interface{}, conn net.Conn, buf []byte, deadline time.Time) error {
	return w.aioCreate(ctx, OpWrite, conn, buf, deadline)
}

// Free let the watcher to release resources related to this conn immediately,
// like socket file descriptors.
func (w *Watcher) Free(conn net.Conn) error {
	return w.aioCreate(nil, opDelete, conn, nil, zeroTime)
}

// core async-io creation
func (w *Watcher) aioCreate(ctx interface{}, op OpType, conn net.Conn, buf []byte, deadline time.Time) error {
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
func (w *Watcher) tryRead(pcb *aiocb) bool {
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
			return false
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
		return true
	case <-w.die:
		return true
	}
}

func (w *Watcher) tryWrite(pcb *aiocb) bool {
	var nw int
	var ew error

	if pcb.buffer != nil {
		nw, ew = syscall.Write(pcb.fd, pcb.buffer[pcb.size:])
		pcb.err = ew
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
		case w.chIOCompletion <- OpResult{Operation: OpWrite, Conn: pcb.conn, Buffer: pcb.buffer, Size: nw, Error: ew, Context: pcb.ctx}:
			return true
		case <-w.die:
			return true
		}
	}
	return false
}

// the core event loop of this watcher
func (w *Watcher) loop() {

	// the maps below is consistent at any give time.
	queuedReaders := make(map[int]*list.List) // ident -> aiocb
	queuedWriters := make(map[int]*list.List)

	// we must not hold net.Conn as key, for GC purpose
	connIdents := make(map[uintptr]int)
	idents := make(map[int]uintptr) // fd->net.conn
	gc := make(chan uintptr)

	// for timeout operations
	timer := time.NewTimer(0)
	var timeouts timedHeap

	releaseConn := func(ident int) {
		//log.Println("release", ident)
		if ptr, ok := idents[ident]; ok {
			delete(queuedReaders, ident)
			delete(queuedWriters, ident)
			delete(idents, ident)
			delete(connIdents, ptr)
			// close socket file descriptor duplicated from net.Conn
			syscall.Close(ident)
		}
	}

	// release all resources
	defer func() {
		for ident := range idents {
			releaseConn(ident)
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

			for _, pcb := range pending {
				ident, ok := connIdents[pcb.ptr]
				// resource release
				if pcb.op == opDelete {
					if ok {
						// for each request, send release signal
						// before resource releases.
						l := queuedReaders[ident]
						for e := l.Front(); e != nil; e = e.Next() {
							tcb := e.Value.(*aiocb)
							select {
							case w.chIOCompletion <- OpResult{Operation: tcb.op, Conn: tcb.conn, Buffer: tcb.buffer, Size: tcb.size, Error: ErrConnClosed, Context: tcb.ctx}:
							case <-w.die:
								return
							}
						}
						l = queuedWriters[ident]
						for e := l.Front(); e != nil; e = e.Next() {
							tcb := e.Value.(*aiocb)
							select {
							case w.chIOCompletion <- OpResult{Operation: tcb.op, Conn: tcb.conn, Buffer: tcb.buffer, Size: tcb.size, Error: ErrConnClosed, Context: tcb.ctx}:
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
						case <-w.die:
							return
						}
						continue
					} else {
						// assign idents
						ident = dupfd

						// unexpected situation, should notify caller
						werr := w.pfd.Watch(ident)
						if werr != nil {
							select {
							case w.chIOCompletion <- OpResult{Operation: pcb.op, Conn: pcb.conn, Buffer: pcb.buffer, Size: 0, Error: werr, Context: pcb.ctx}:
							case <-w.die:
								return
							}
							continue
						}

						// bindings
						idents[ident] = pcb.ptr
						connIdents[pcb.ptr] = ident
						queuedReaders[ident] = new(list.List)
						queuedWriters[ident] = new(list.List)
						// as we duplicated succesfuly, we're safe to
						// close the original connection
						pcb.conn.Close()

						// the conn is still useful for GC finalizer
						// note finalizer function cannot hold reference to net.Conn
						// if not it will never be GC-ed
						runtime.SetFinalizer(pcb.conn, func(c net.Conn) {
							select {
							case gc <- reflect.ValueOf(c).Pointer():
							case <-w.die:
							}
						})
					}
				}
				//log.Println("idents:", ident, ptr)
				pcb.fd = ident

				// operations splitted into different buckets
				switch pcb.op {
				case OpRead:
					l := queuedReaders[ident]
					if l.Len() == 0 {
						if w.tryRead(pcb) {
							if pcb.err != nil || (pcb.size == 0 && pcb.err == nil) {
								releaseConn(ident)
							}
							continue
						}
					}
					pcb.l = l
					pcb.elem = pcb.l.PushBack(pcb)
				case OpWrite:
					l := queuedWriters[ident]
					if l.Len() == 0 {
						if w.tryWrite(pcb) {
							if pcb.err != nil {
								releaseConn(ident)
							}
							continue
						}
					}
					pcb.l = l
					pcb.elem = pcb.l.PushBack(pcb)
				}

				// timer
				if !pcb.deadline.IsZero() {
					heap.Push(&timeouts, pcb)
					if timeouts.Len() == 1 {
						timer.Reset(pcb.deadline.Sub(time.Now()))
					}
				}
			}
			pending = pending[:0]
		case pe := <-w.chEventNotify:
			// suppose fd(s) being polled is closed by conn.Close() from outside after chanrecv,
			// and a new conn has re-opened with the same handler number(fd). The read and write
			// on this fd is fatal.
			//
			// Note poller will remove closed fd automatically epoll(7), kqueue(2) and silently.
			// To solve this problem watcher will dup() a new fd from net.Conn, which uniquely
			// identified by 'e.ident', all library operation will be based on 'e.ident',
			// then IO operation is impossible to misread or miswrite on re-created fd.
			//log.Println(e)
			for _, e := range pe {
				if e.r {
					if l := queuedReaders[e.ident]; l != nil {
						var next *list.Element
						for elem := l.Front(); elem != nil; elem = next {
							next = elem.Next()
							pcb := elem.Value.(*aiocb)
							if w.tryRead(pcb) {
								l.Remove(elem)
								if !pcb.deadline.IsZero() {
									heap.Remove(&timeouts, pcb.idx)
								}
								if pcb.err != nil || (pcb.size == 0 && pcb.err == nil) {
									releaseConn(e.ident)
									break
								}
							} else {
								break
							}
						}
					}
				}

				if e.w {
					if l := queuedWriters[e.ident]; l != nil {
						var next *list.Element
						for elem := l.Front(); elem != nil; elem = next {
							next = elem.Next()
							pcb := elem.Value.(*aiocb)
							if w.tryWrite(pcb) {
								l.Remove(elem)
								if !pcb.deadline.IsZero() {
									heap.Remove(&timeouts, pcb.idx)
								}
								if pcb.err != nil {
									releaseConn(e.ident)
									break
								}
							} else {
								break
							}
						}
					}
				}
			}
		case <-timer.C:
			for timeouts.Len() > 0 {
				now := time.Now()
				pcb := timeouts[0]
				if now.After(pcb.deadline) {
					if _, ok := connIdents[pcb.ptr]; ok {
						// remove from list
						pcb.l.Remove(pcb.elem)
						// ErrDeadline
						select {
						case w.chIOCompletion <- OpResult{Operation: pcb.op, Conn: pcb.conn, Buffer: pcb.buffer, Size: pcb.size, Error: ErrDeadline, Context: pcb.ctx}:
						case <-w.die:
							return
						}
					}
					heap.Pop(&timeouts)
				} else {
					timer.Reset(pcb.deadline.Sub(now))
					break
				}
			}
		case ptr := <-gc: // gc recycled net.Conn
			if ident, ok := connIdents[ptr]; ok {
				// since it's gc-ed, queue is impossible to hold net.Conn
				// we don't have to send to chIOCompletion,just release here
				releaseConn(ident)
			}
		case <-w.die:
			return
		}
	}
}
