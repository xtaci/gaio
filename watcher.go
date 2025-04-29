// The MIT License (MIT)
//
// Copyright (c) 2019 xtaci
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package gaio

import (
	"container/heap"
	"container/list"
	"io"
	"net"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	aiocbPool sync.Pool
)

func init() {
	aiocbPool.New = func() interface{} {
		return new(aiocb)
	}
}

// fdDesc holds all data structures associated with a file descriptor (fd).
// It maintains lists of pending read and write requests, as well as a pointer
// to the associated net.Conn object.
type fdDesc struct {
	readers list.List // List of pending read requests
	writers list.List // List of pending write requests
	ptr     uintptr   // Pointer to the associated net.Conn object (stored as uintptr for GC safety)
}

// watcher is responsible for monitoring file descriptors, handling events,
// and processing asynchronous I/O requests. It manages event polling,
// maintains internal buffers, and interacts with various channels for signaling
// and communication.
type watcher struct {
	// poll fd
	pfd *poller // Poller for managing file descriptor events

	// netpoll signals
	chSignal chan Signal

	// Lists for managing pending asynchronous I/O operations
	// pendingXXX will be used interchangeably, like back buffer
	// pendingCreate <--> pendingProcessing
	chPendingNotify   chan struct{} // Channel for notifications about new I/O requests
	pendingCreate     []*aiocb      // List of I/O operations waiting to be processed
	pendingProcessing []*aiocb      // List of I/O operations currently under processing

	pendingMutex sync.Mutex // Mutex to synchronize access to pending operations
	recycles     []*aiocb   // List of completed I/O operations ready for reuse

	// IO-completion events to user
	chResults chan *aiocb

	// Internal buffers for managing read operations
	swapSize         int    // Capacity of the swap buffer (triple buffer system)
	swapBufferFront  []byte // Front buffer for reading
	swapBufferMiddle []byte // Middle buffer for reading
	swapBufferBack   []byte // Back buffer for reading
	bufferOffset     int    // Offset for the currently used buffer
	shouldSwap       int32  // Atomic flag indicating if a buffer swap is needed

	// Channel for setting CPU affinity in the watcher loop
	chCPUID chan int32

	// Maps and structures for managing file descriptors and connections
	descs      map[int]*fdDesc // Map of file descriptors to their associated fdDesc
	connIdents map[uintptr]int // Map of net.Conn pointers to unique identifiers (avoids GC issues)
	timeouts   timedHeap       // Heap for managing requests with timeouts
	timer      *time.Timer     // Timer for handling timeouts

	// Garbage collection
	gc       []net.Conn    // List of connections to be garbage collected
	gcMutex  sync.Mutex    // Mutex to synchronize access to the gc list
	gcNotify chan struct{} // Channel to notify the GC processor
	gcFound  uint32        // number of net.Conn objects found unreachable by runtime
	gcClosed uint32        // record number of objects closed successfully

	// Shutdown and cleanup
	die     chan struct{} // Channel for signaling shutdown
	dieOnce sync.Once     // Ensures that the watcher is only closed once
}

// NewWatcher creates a new Watcher instance with a default internal buffer size of 64KB.
func NewWatcher() (*Watcher, error) {
	return NewWatcherSize(defaultInternalBufferSize)
}

// NewWatcherSize creates a new Watcher instance with a specified internal buffer size.
//
// It allocates three shared buffers of the given size for handling read requests.
// This allows efficient management of read operations by using pre-allocated buffers.
func NewWatcherSize(bufsize int) (*Watcher, error) {
	w := new(watcher)

	// Initialize the poller for managing file descriptor events
	pfd, err := openPoll()
	if err != nil {
		return nil, err
	}
	w.pfd = pfd

	// Initialize channels for communication and signaling
	w.chCPUID = make(chan int32)
	w.chSignal = make(chan Signal, 1)
	w.chPendingNotify = make(chan struct{}, 1)
	w.chResults = make(chan *aiocb, maxEvents*4)
	w.die = make(chan struct{})

	// Allocate and initialize buffers for shared reading operations
	w.swapSize = bufsize
	w.swapBufferFront = make([]byte, bufsize)
	w.swapBufferMiddle = make([]byte, bufsize)
	w.swapBufferBack = make([]byte, bufsize)

	// Initialize data structures for managing file descriptors and connections
	w.descs = make(map[int]*fdDesc)
	w.connIdents = make(map[uintptr]int)
	w.gcNotify = make(chan struct{}, 1)
	w.timer = time.NewTimer(0)

	// Start background goroutines for netpoll and main loop
	go w.pfd.Wait(w.chSignal)
	go w.loop()

	// Set up a finalizer to ensure resources are cleaned up when the Watcher is garbage collected
	// NOTE: we need a manual garbage collection mechanism for watcher
	wrapper := &Watcher{watcher: w}
	runtime.SetFinalizer(wrapper, func(wrapper *Watcher) {
		wrapper.Close()
	})

	return wrapper, nil
}

// Set Poller Affinity for Epoll/Kqueue
func (w *watcher) SetPollerAffinity(cpuid int) (err error) {
	if cpuid >= runtime.NumCPU() {
		return ErrCPUID
	}

	// store and wakeup
	atomic.StoreInt32(&w.pfd.cpuid, int32(cpuid))
	w.pfd.wakeup()
	return nil
}

// Set Loop Affinity for syscall.Read/syscall.Write
func (w *watcher) SetLoopAffinity(cpuid int) (err error) {
	if cpuid >= runtime.NumCPU() {
		return ErrCPUID
	}

	// sendchan
	select {
	case w.chCPUID <- int32(cpuid):
	case <-w.die:
		return ErrConnClosed
	}
	return nil
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

// WaitIO blocks until one or more read/write operations are completed or an error occurs.
// It returns a slice of OpResult containing details of completed operations and any errors encountered.
//
// The method operates as follows:
// 1. It recycles previously used aiocb objects to avoid memory leaks and reuse them for new I/O operations.
// 2. It waits for completion notifications from the chResults channel and accumulates results.
// 3. It ensures that the buffer in OpResult is not overwritten until the next call to WaitIO.
func (w *watcher) WaitIO() (r []OpResult, err error) {
	// recycle previous aiocb
	for k := range w.recycles {
		aiocbPool.Put(w.recycles[k])
		// avoid memory leak
		w.recycles[k] = nil
	}
	w.recycles = w.recycles[:0]

	for {
		select {
		case pcb := <-w.chResults:
			r = append(r, OpResult{Operation: pcb.op, Conn: pcb.conn, IsSwapBuffer: pcb.useSwap, Buffer: pcb.buffer, Size: pcb.size, Error: pcb.err, Context: pcb.ctx})
			// avoid memory leak
			pcb.ctx = nil
			w.recycles = append(w.recycles, pcb)
			for len(w.chResults) > 0 {
				pcb := <-w.chResults
				r = append(r, OpResult{Operation: pcb.op, Conn: pcb.conn, IsSwapBuffer: pcb.useSwap, Buffer: pcb.buffer, Size: pcb.size, Error: pcb.err, Context: pcb.ctx})
				// avoid memory leak
				pcb.ctx = nil
				w.recycles = append(w.recycles, pcb)
			}

			// The buffer swapping mechanism ensures that the 'Buffer' in the returned OpResult
			// is not overwritten until the next call to WaitIO. This allows the user to safely
			// access the buffer without worrying about it being modified by subsequent operations.
			//
			// We use a triple buffer system to manage the buffers efficiently. This system
			// maintains three types of buffer states during operations:
			//
			// 1. **DONE**: Results are fully processed and accessible to the user.
			// 2. **INFLIGHT**: Results are completed but still being delivered to chResults.
			// 3. **WRITING**: Results are being written to the next buffer.
			//
			// 	T0: DONE(B0) | INFLIGHT DELIVERY(B0)
			// switching to B1
			// 	T0': WRITING(B1)
			//
			// 	T1: DONE(B0+B1) | INFLIGHT DELIVERY(B1)
			// switching to B2
			// 	T1': WRITING(B2)
			//
			// 	T2: DONE(B1+B2) | INFLIGHT DELIVERY(B2)
			// switching to B0
			//	T2': WRITING(B0)
			// - and so on...
			//
			// Atomic operation ensures synchronization for buffer swapping.
			atomic.CompareAndSwapInt32(&w.shouldSwap, 0, 1)

			return r, nil
		case <-w.die:
			return nil, ErrWatcherClosed
		}
	}
}

// Read submits an asynchronous read request on 'conn' with context 'ctx' and optional buffer 'buf'.
// If 'buf' is nil, an internal buffer is used. 'ctx' is a user-defined value passed unchanged.
func (w *watcher) Read(ctx interface{}, conn net.Conn, buf []byte) error {
	return w.aioCreate(ctx, OpRead, conn, buf, zeroTime, false)
}

// ReadTimeout submits an asynchronous read request on 'conn' with context 'ctx' and buffer 'buf',
// expecting to read some bytes before 'deadline'. 'ctx' is a user-defined value passed unchanged.
func (w *watcher) ReadTimeout(ctx interface{}, conn net.Conn, buf []byte, deadline time.Time) error {
	return w.aioCreate(ctx, OpRead, conn, buf, deadline, false)
}

// ReadFull submits an asynchronous read request on 'conn' with context 'ctx' and buffer 'buf',
// expecting to fill the buffer before 'deadline'. 'ctx' is a user-defined value passed unchanged.
// 'buf' must not be nil for ReadFull.
func (w *watcher) ReadFull(ctx interface{}, conn net.Conn, buf []byte, deadline time.Time) error {
	if len(buf) == 0 {
		return ErrEmptyBuffer
	}
	return w.aioCreate(ctx, OpRead, conn, buf, deadline, true)
}

// Write submits an asynchronous write request on 'conn' with context 'ctx' and buffer 'buf'.
// 'ctx' is a user-defined value passed unchanged.
func (w *watcher) Write(ctx interface{}, conn net.Conn, buf []byte) error {
	if len(buf) == 0 {
		return ErrEmptyBuffer
	}
	return w.aioCreate(ctx, OpWrite, conn, buf, zeroTime, false)
}

// WriteTimeout submits an asynchronous write request on 'conn' with context 'ctx' and buffer 'buf',
// expecting to complete writing before 'deadline'. 'ctx' is a user-defined value passed unchanged.
func (w *watcher) WriteTimeout(ctx interface{}, conn net.Conn, buf []byte, deadline time.Time) error {
	if len(buf) == 0 {
		return ErrEmptyBuffer
	}
	return w.aioCreate(ctx, OpWrite, conn, buf, deadline, false)
}

// Free releases resources related to 'conn' immediately, such as socket file descriptors.
func (w *watcher) Free(conn net.Conn) error {
	return w.aioCreate(nil, opDelete, conn, nil, zeroTime, false)
}

// aioCreate initiates an asynchronous IO operation with the given parameters.
// It creates an aiocb structure and adds it to the pending queue, then notifies the watcher.
func (w *watcher) aioCreate(ctx interface{}, op OpType, conn net.Conn, buf []byte, deadline time.Time, readfull bool) error {
	select {
	case <-w.die:
		return ErrWatcherClosed
	default:
		var ptr uintptr
		if conn != nil && reflect.TypeOf(conn).Kind() == reflect.Ptr {
			ptr = reflect.ValueOf(conn).Pointer()
		} else {
			return ErrUnsupported
		}

		cb := aiocbPool.Get().(*aiocb)
		*cb = aiocb{op: op, ptr: ptr, size: 0, ctx: ctx, conn: conn, buffer: buf, deadline: deadline, readFull: readfull, idx: -1}

		w.pendingMutex.Lock()
		w.pendingCreate = append(w.pendingCreate, cb)
		w.pendingMutex.Unlock()

		w.notifyPending()
		return nil
	}
}

// tryRead attempts to read data on aiocb and notify the completion.
// Returns true if the operation is completed; false if it is not completed and will retry later.
func (w *watcher) tryRead(fd int, pcb *aiocb) bool {
	// step 1. bind to proper buffer
	buf := pcb.buffer

	useSwap := false
	backBuffer := false

	if buf == nil {
		if atomic.CompareAndSwapInt32(&w.shouldSwap, 1, 0) {
			// A successful CAS operation triggers internal buffer swapping:
			//
			// Initial State:
			//
			// +-------+    +--------+    +------+
			// | Front | -> | Middle | -> | Back |
			// +-------+    +--------+    +------+
			//      |                        ^
			//      |________________________|
			//
			// After One Circular Shift:
			//
			// +--------+    +------+    +-------+
			// | Middle | -> | Back | -> | Front |
			// +--------+    +------+    +-------+
			//      |                        ^
			//      |________________________|
			//
			// After Two Circular Shifts:
			//
			// +------+    +-------+    +--------+
			// | Back | -> | Front | -> | Middle |
			// +------+    +-------+    +--------+
			//      |                        ^
			//      |________________________|
			w.swapBufferFront, w.swapBufferMiddle, w.swapBufferBack = w.swapBufferMiddle, w.swapBufferBack, w.swapBufferFront
			w.bufferOffset = 0
		}

		buf = w.swapBufferFront[w.bufferOffset:]
		if len(buf) > 0 {
			useSwap = true
		} else {
			backBuffer = true
			buf = pcb.backBuffer[:]
		}
	}

	// step 2. read into buffer
	for {
		nr, er := rawRead(fd, buf[pcb.size:])
		if er == syscall.EAGAIN {
			return false
		}

		// On MacOS we can see EINTR here if the user
		// pressed ^Z.
		if er == syscall.EINTR {
			continue
		}

		// if er is nil, accumulate bytes read
		if er == nil {
			pcb.size += nr
		}

		pcb.err = er
		// proper setting of EOF
		if nr == 0 && er == nil {
			pcb.err = io.EOF
		}

		break
	}

	// step 3.check read full operation
	// 	the buffer of readfull operation is guaranteed from caller
	if pcb.readFull { // read full operation
		if pcb.err != nil {
			// the operation is completed due to error
			return true
		}

		if pcb.size == len(pcb.buffer) {
			// the operation is completed normally
			return true
		}
		return false
	}

	// step 4. non read-full operations
	if useSwap { // IO completed with internal buffer
		pcb.useSwap = true
		pcb.buffer = buf[:pcb.size] // set len to pcb.size
		w.bufferOffset += pcb.size
	} else if backBuffer { // use per request tiny buffer
		pcb.buffer = buf
	}
	return true
}

// tryWrite attempts to write data on aiocb and notifies the completion.
// Returns true if the operation is completed; false if it is not completed and will retry later.
func (w *watcher) tryWrite(fd int, pcb *aiocb) bool {
	var nw int
	var ew error

	if pcb.buffer != nil {
		for {
			nw, ew = rawWrite(fd, pcb.buffer[pcb.size:])
			pcb.err = ew

			// Socket buffer is full
			if ew == syscall.EAGAIN {
				return false
			}

			// On MacOS/BSDs, if mbufs ran out, ENOBUFS will be returned
			// https://man.freebsd.org/cgi/man.cgi?query=mbuf&sektion=9&format=html
			if ew == syscall.ENOBUFS {
				return false
			}

			// On MacOS we can see EINTR here if the user pressed ^Z.
			if ew == syscall.EINTR {
				continue
			}

			// If no error, accumulate bytes written
			if ew == nil {
				pcb.size += nw
			}
			break
		}
	}

	// Returns true if all bytes are written or there are errors on the socket
	if pcb.size == len(pcb.buffer) || ew != nil {
		return true
	}

	// Should retry later
	return false
}

// releaseConn releases resources related to the connection identified by 'ident'.
func (w *watcher) releaseConn(ident int) {
	if desc, ok := w.descs[ident]; ok {
		// Remove all pending read requests
		for e := desc.readers.Front(); e != nil; e = e.Next() {
			tcb := e.Value.(*aiocb)
			// Notify caller with error
			tcb.err = io.ErrClosedPipe
			w.deliver(tcb)
		}

		// Remove all pending write requests
		for e := desc.writers.Front(); e != nil; e = e.Next() {
			tcb := e.Value.(*aiocb)
			// Notify caller with error
			tcb.err = io.ErrClosedPipe
			w.deliver(tcb)
		}

		// Purge the fdDesc
		delete(w.descs, ident)
		delete(w.connIdents, desc.ptr)

		// Close the socket file descriptor duplicated from net.Conn
		syscall.Close(ident)
	}
}

// deliver sends the aiocb to the user to retrieve the results.
func (w *watcher) deliver(pcb *aiocb) {
	if pcb.idx != -1 {
		heap.Remove(&w.timeouts, pcb.idx)
	}

	select {
	case w.chResults <- pcb:
	case <-w.die:
	}
}

// loop is the core event loop of the watcher, handling various events and tasks.
func (w *watcher) loop() {
	// Defer function to release all resources
	defer func() {
		for ident := range w.descs {
			w.releaseConn(ident)
		}
	}()

	for {
		select {
		case <-w.chPendingNotify:
			// Swap w.pendingCreate with w.pendingProcessing
			w.pendingMutex.Lock()
			w.pendingCreate, w.pendingProcessing = w.pendingProcessing, w.pendingCreate
			for i := 0; i < len(w.pendingCreate); i++ {
				w.pendingCreate[i] = nil
			}
			w.pendingCreate = w.pendingCreate[:0]
			w.pendingMutex.Unlock()

			// handlePending is a synchronous operation to process all pending requests
			w.handlePending(w.pendingProcessing)

		case sig := <-w.chSignal: // Poller events
			w.handleEvents(sig.events)
			select {
			case sig.done <- struct{}{}:
			case <-w.die:
				return
			}

		case <-w.timer.C: //  a global timeout heap to handle all timeouts
			for w.timeouts.Len() > 0 {
				now := time.Now()
				pcb := w.timeouts[0]
				if now.After(pcb.deadline) {
					// ErrDeadline
					pcb.err = ErrDeadline
					// remove from list
					pcb.l.Remove(pcb.elem)
					// deliver with error: ErrDeadline
					w.deliver(pcb)
				} else {
					w.timer.Reset(pcb.deadline.Sub(now))
					break
				}
			}
		case <-w.gcNotify:
			w.handleGC()
		case cpuid := <-w.chCPUID:
			setAffinity(cpuid)

		case <-w.die:
			return
		}
	}
}

// handleGC processes the garbage collection of net.Conn objects.
func (w *watcher) handleGC() {
	runtime.GC()
	w.gcMutex.Lock()
	if len(w.gc) > 0 {
		for _, c := range w.gc {
			ptr := reflect.ValueOf(c).Pointer()
			if ident, ok := w.connIdents[ptr]; ok {
				w.releaseConn(ident)
			}
		}
		w.gcClosed += uint32(len(w.gc))
		w.gc = w.gc[:0]
	}
	w.gcMutex.Unlock()
}

// handlePending processes new requests, acting as a reception desk.
func (w *watcher) handlePending(pending []*aiocb) {
PENDING:
	for _, pcb := range pending {
		ident, ok := w.connIdents[pcb.ptr]
		// Resource releasing operation
		if pcb.op == opDelete && ok {
			w.releaseConn(ident)
			continue
		}

		// Check if the file descriptor is already registered
		var desc *fdDesc
		if ok {
			desc = w.descs[ident]
		} else {
			// New file descriptor registration
			if dupfd, err := dupconn(pcb.conn); err != nil {
				// unexpected situation, should notify caller if we cannot dup(2)
				pcb.err = err
				w.deliver(pcb)
				continue
			} else {
				// as we duplicated successfully, we're safe to
				// close the original connection
				pcb.conn.Close()
				// assign idents
				ident = dupfd

				// let epoll or kqueue to watch this fd!
				werr := w.pfd.Watch(ident)
				if werr != nil {
					// unexpected situation, should notify caller if we cannot watch
					pcb.err = werr
					w.deliver(pcb)
					continue
				}

				// update registration table
				desc = &fdDesc{ptr: pcb.ptr}
				w.descs[ident] = desc
				w.connIdents[pcb.ptr] = ident

				// the 'conn' object is still useful for GC finalizer.
				// note finalizer function cannot hold reference to net.Conn,
				// if not it will never be GC-ed.
				runtime.SetFinalizer(pcb.conn, func(c net.Conn) {
					w.gcMutex.Lock()
					w.gc = append(w.gc, c)
					w.gcFound++
					w.gcMutex.Unlock()

					// notify gc processor
					select {
					case w.gcNotify <- struct{}{}:
					default:
					}
				})
			}
		}

		// as the file descriptor is registered, we can proceed to IO operations
		switch pcb.op {
		case OpRead:
			// if there's no pending read requests
			// we can try to read immediately
			if desc.readers.Len() == 0 {
				if w.tryRead(ident, pcb) {
					w.deliver(pcb)
					// request fulfilled, continue to next
					continue PENDING
				}
			}

			// if the request is not fulfilled, we should queue it
			pcb.l = &desc.readers
			pcb.elem = pcb.l.PushBack(pcb)

		case OpWrite:
			if desc.writers.Len() == 0 {
				if w.tryWrite(ident, pcb) {
					w.deliver(pcb)
					continue PENDING
				}
			}

			pcb.l = &desc.writers
			pcb.elem = pcb.l.PushBack(pcb)
		}

		// if the request has deadline set, we should push it to timeout heap
		if !pcb.deadline.IsZero() {
			heap.Push(&w.timeouts, pcb)
			if w.timeouts.Len() == 1 {
				w.timer.Reset(time.Until(pcb.deadline))
			}
		}
	}
}

// handleEvents processes a batch of poller events and manages I/O operations for the associated file descriptors.
// Each event contains information about file descriptor activity, and the handler ensures that read and write
// operations are completed correctly even if the file descriptor has been re-opened after being closed.
//
// Note: If a file descriptor is closed externally (e.g., by conn.Close()), and then re-opened with the same
// handler number (fd), operations on the old fd can lead to errors. To handle this, the watcher duplicates the
// file descriptor from net.Conn, and operations are based on the unique identifier 'e.ident'. This prevents
// misreading or miswriting on re-created file descriptors.
//
// The poller automatically removes closed file descriptors from the event poller (epoll(7), kqueue(2)), so we
// need to handle these events correctly and ensure that all pending operations are processed.
func (w *watcher) handleEvents(events pollerEvents) {
	for _, e := range events {
		if desc, ok := w.descs[e.ident]; ok {
			// Process read events if the event indicates a read operation
			if e.ev&EV_READ != 0 {
				var next *list.Element
				// try to complete all read requests
				for elem := desc.readers.Front(); elem != nil; elem = next {
					next = elem.Next()
					pcb := elem.Value.(*aiocb)
					if w.tryRead(e.ident, pcb) {
						w.deliver(pcb)            // Deliver the completed read operation
						desc.readers.Remove(elem) // Remove the completed read request from the queue
					} else {
						// Stop processing further read requests if the current read operation fails
						break
					}
				}
			}

			// Process write events if the event indicates a write operation
			if e.ev&EV_WRITE != 0 {
				var next *list.Element
				for elem := desc.writers.Front(); elem != nil; elem = next {
					next = elem.Next()
					pcb := elem.Value.(*aiocb)
					if w.tryWrite(e.ident, pcb) {
						w.deliver(pcb)
						desc.writers.Remove(elem)
					} else {
						break
					}
				}

			}
		}
	}
}

// read gcFound & gcClosed
func (w *watcher) GetGC() (found uint32, closed uint32) {
	w.gcMutex.Lock()
	defer w.gcMutex.Unlock()
	return w.gcFound, w.gcClosed
}
