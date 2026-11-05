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
	aiocbPool = sync.Pool{
		New: func() any { return new(aiocb) },
	}
	fdDescPool = sync.Pool{
		New: func() any { return new(fdDesc) },
	}
)

// fdDesc stores the pending read and write requests for a registered file
// descriptor, along with the identity of its associated net.Conn.
type fdDesc struct {
	readers list.List // pending read requests
	writers list.List // pending write requests
	ptr     uintptr   // identity of the associated net.Conn, stored without keeping it alive
}

// watcher owns the poller and coordinates asynchronous requests, completions,
// deadlines, shared read buffers, and connection cleanup.
type watcher struct {
	// Poller used to monitor registered file descriptors.
	pfd *poller

	// Readiness events sent from the poller to the watcher loop.
	chSignal chan Signal

	// Requests are appended to pendingCreate and batch-processed from
	// pendingProcessing. The slices are swapped to avoid copying.
	chPendingNotify   chan struct{} // Channel for notifications about new I/O requests
	pendingCreate     []*aiocb      // List of I/O operations waiting to be processed
	pendingProcessing []*aiocb      // List of I/O operations currently under processing

	pendingMutex sync.Mutex // Mutex to synchronize access to pending operations
	recycles     []*aiocb   // List of completed I/O operations ready for reuse

	// I/O completion events for the user
	chResults chan *aiocb

	// Three rotating buffers back read requests that do not provide a buffer.
	swapSize         int           // Capacity of the swap buffer (triple buffer system)
	swapBufferFront  []byte        // Front buffer for reading
	swapBufferMiddle []byte        // Middle buffer for reading
	swapBufferBack   []byte        // Back buffer for reading
	bufferOffset     int           // Offset for the currently used buffer
	shouldSwap       chan struct{} // Notification channel indicating a buffer swap is needed

	// Requests a CPU-affinity change for the watcher loop.
	chCPUID chan int32

	// Pre-allocated results slice to reduce allocations in WaitIO
	results []OpResult

	// Registration state indexed by socket handle and connection identity.
	descs      map[int]*fdDesc // socket handle to its pending-operation state
	connIdents map[uintptr]int // connection identity to its registered socket handle
	timeouts   timedHeap       // Heap for managing requests with timeouts
	timer      *time.Timer     // Timer for handling timeouts

	// Connections queued by finalizers for release by the watcher loop.
	gc       []net.Conn    // List of connections to be garbage collected
	gcMutex  sync.Mutex    // Mutex to synchronize access to the gc list
	gcNotify chan struct{} // Channel to notify the GC processor
	gcFound  uint32        // Number of net.Conn objects found unreachable by the runtime
	gcClosed uint32        // Number of objects closed successfully

	// Shutdown coordination.
	die       chan struct{} // Channel for signaling shutdown
	dieOnce   sync.Once     // Ensures that the watcher is only closed once
	loopDone  chan struct{} // Closed after the watcher loop releases its resources
	pollerErr error         // Terminal poller error, written before loopDone closes
}

// NewWatcher creates a watcher with the default 64 KiB internal read-buffer size.
func NewWatcher() (*Watcher, error) {
	return NewWatcherSize(defaultInternalBufferSize)
}

// NewWatcherSize creates a watcher with the specified internal read-buffer size.
//
// Three shared buffers of this size are allocated for read requests submitted
// with a nil buffer.
func NewWatcherSize(bufsize int) (*Watcher, error) {
	w := new(watcher)

	// Create the platform-specific poller.
	pfd, err := openPoll()
	if err != nil {
		return nil, err
	}
	w.pfd = pfd

	// Set up channels used by the poller and watcher loop.
	w.chCPUID = make(chan int32)
	w.chSignal = make(chan Signal, 1)
	w.chPendingNotify = make(chan struct{}, 1)
	w.chResults = make(chan *aiocb, maxEvents*4)
	w.die = make(chan struct{})
	w.loopDone = make(chan struct{})

	// Allocate the shared buffers used by nil-buffer reads.
	w.swapSize = bufsize
	w.swapBufferFront = make([]byte, bufsize)
	w.swapBufferMiddle = make([]byte, bufsize)
	w.swapBufferBack = make([]byte, bufsize)
	w.shouldSwap = make(chan struct{}, 1)

	// Preallocate request and result slices for the common case.
	w.pendingCreate = make([]*aiocb, 0, 128)
	w.pendingProcessing = make([]*aiocb, 0, 128)
	w.recycles = make([]*aiocb, 0, 128)
	w.results = make([]OpResult, 0, 128)

	// Initialize registration tables and finalizer state.
	w.descs = make(map[int]*fdDesc, 1024)
	w.connIdents = make(map[uintptr]int, 1024)
	w.gc = make([]net.Conn, 0, 64)
	w.gcNotify = make(chan struct{}, 1)

	// Keep the timer stopped until a request deadline needs to be scheduled.
	w.timer = time.NewTimer(time.Hour)
	if !w.timer.Stop() {
		select {
		case <-w.timer.C:
		default:
		}
	}

	// Start the poller and the request-processing loop.
	go w.pfd.Wait(w.chSignal)
	go w.loop()

	// Close the watcher if its wrapper becomes unreachable without an explicit
	// call to Close.
	wrapper := &Watcher{watcher: w}
	runtime.SetFinalizer(wrapper, func(wrapper *Watcher) {
		wrapper.Close()
	})

	return wrapper, nil
}

// SetPollerAffinity requests CPU affinity for the epoll/kqueue poller.
func (w *watcher) SetPollerAffinity(cpuid int) error {
	if cpuid < 0 || cpuid >= runtime.NumCPU() {
		return ErrCPUID
	}

	// Store the request and wake the poller so it can apply the new affinity.
	atomic.StoreInt32(&w.pfd.cpuid, int32(cpuid))
	if err := w.pfd.wakeup(); err != nil {
		// Clear the request if the poller could not be woken.
		atomic.StoreInt32(&w.pfd.cpuid, -1)
		return err
	}
	return nil
}

// SetLoopAffinity requests CPU affinity for the watcher's I/O loop.
func (w *watcher) SetLoopAffinity(cpuid int) error {
	if cpuid < 0 || cpuid >= runtime.NumCPU() {
		return ErrCPUID
	}

	// The loop applies the requested CPU when it receives this value.
	select {
	case w.chCPUID <- int32(cpuid):
	case <-w.die:
		return ErrConnClosed
	}
	return nil
}

// Close shuts down the watcher and stops monitoring its connections.
// Pending operations are canceled; WaitIO returns ErrWatcherClosed rather than
// a completion result for each canceled operation.
func (w *watcher) Close() (err error) {
	err = w.stop()
	<-w.loopDone
	return err
}

func (w *watcher) stop() (err error) {
	w.dieOnce.Do(func() {
		w.pendingMutex.Lock()
		close(w.die)
		w.pendingMutex.Unlock()
		err = w.pfd.Close()
	})
	return err
}

// notifyPending wakes the loop to process newly submitted operations.
func (w *watcher) notifyPending() {
	select {
	case w.chPendingNotify <- struct{}{}:
	default:
	}
}

// notifyShouldSwap requests a rotation of the shared read buffers.
func (w *watcher) notifyShouldSwap() {
	select {
	case w.shouldSwap <- struct{}{}:
	default:
	}
}

// WaitIO blocks until at least one operation completes or the watcher closes.
// It returns the completed operations in a reusable slice of OpResult.
//
// Completed requests already queued when the first result arrives are included
// in the same result batch. Request records from the previous call are recycled.
// The returned slice and its internal read buffers remain valid until the next
// call to WaitIO.
func (w *watcher) WaitIO() (r []OpResult, err error) {
	// Recycle previous aiocb objects using batch put for better performance
	for _, cb := range w.recycles {
		aiocbPool.Put(cb)
	}
	// Clear the slice to avoid memory leaks while keeping capacity
	clear(w.recycles)
	w.recycles = w.recycles[:0]

	// Clear previous results to allow GC of Conn objects
	// This is important because the underlying array may still hold references
	for i := range w.results {
		w.results[i].Conn = nil
		w.results[i].Context = nil
		w.results[i].Buffer = nil
	}
	w.results = w.results[:0]

	for {
		select {
		case pcb := <-w.chResults:
			w.results = append(w.results, OpResult{Operation: pcb.op, Conn: pcb.conn, IsSwapBuffer: pcb.useSwap, Buffer: pcb.buffer, Size: pcb.size, Error: pcb.err, Context: pcb.ctx})
			// Clear references to allow GC of connection objects
			pcb.ctx = nil
			pcb.conn = nil
			w.recycles = append(w.recycles, pcb)
			for len(w.chResults) > 0 {
				pcb := <-w.chResults
				w.results = append(w.results, OpResult{Operation: pcb.op, Conn: pcb.conn, IsSwapBuffer: pcb.useSwap, Buffer: pcb.buffer, Size: pcb.size, Error: pcb.err, Context: pcb.ctx})
				// Clear references to allow GC of connection objects
				pcb.ctx = nil
				pcb.conn = nil
				w.recycles = append(w.recycles, pcb)
			}

			// Keep buffers referenced by this result batch intact until the next
			// WaitIO call.
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
			// Allow subsequent nil-buffer reads to rotate to the next buffer.
			w.notifyShouldSwap()

			return w.results, nil
		case <-w.die:
			if w.pollerErr != nil {
				return nil, w.pollerErr
			}
			return nil, ErrWatcherClosed
		case <-w.loopDone:
			if w.pollerErr != nil {
				return nil, w.pollerErr
			}
			return nil, ErrWatcherClosed
		}
	}
}

// Read submits an asynchronous read request on conn. If buf is nil, the watcher
// uses an internal buffer. ctx is returned unchanged in the corresponding result.
func (w *watcher) Read(ctx any, conn net.Conn, buf []byte) error {
	return w.aioCreate(ctx, OpRead, conn, buf, zeroTime, false)
}

// ReadTimeout submits an asynchronous read request. If deadline is non-zero,
// the request expires if it has not completed by that time. A nil buf selects
// an internal buffer; ctx is returned unchanged in the corresponding result.
func (w *watcher) ReadTimeout(ctx any, conn net.Conn, buf []byte, deadline time.Time) error {
	return w.aioCreate(ctx, OpRead, conn, buf, deadline, false)
}

// ReadFull submits an asynchronous read request that fills buf or completes
// with an error. If deadline is non-zero, it expires if the buffer is not
// filled in time. buf must be non-empty; ctx is returned unchanged in the result.
func (w *watcher) ReadFull(ctx any, conn net.Conn, buf []byte, deadline time.Time) error {
	if len(buf) == 0 {
		return ErrEmptyBuffer
	}
	return w.aioCreate(ctx, OpRead, conn, buf, deadline, true)
}

// Write submits an asynchronous request to write buf to conn. ctx is returned
// unchanged in the corresponding result.
func (w *watcher) Write(ctx any, conn net.Conn, buf []byte) error {
	if len(buf) == 0 {
		return ErrEmptyBuffer
	}
	return w.aioCreate(ctx, OpWrite, conn, buf, zeroTime, false)
}

// WriteTimeout submits an asynchronous request to write buf. If deadline is
// non-zero, the request expires if the write has not completed by that time.
// ctx is returned unchanged in the corresponding result.
func (w *watcher) WriteTimeout(ctx any, conn net.Conn, buf []byte, deadline time.Time) error {
	if len(buf) == 0 {
		return ErrEmptyBuffer
	}
	return w.aioCreate(ctx, OpWrite, conn, buf, deadline, false)
}

// Free queues conn for release by the watcher. Pending operations are completed
// with io.ErrClosedPipe when the connection is released.
func (w *watcher) Free(conn net.Conn) error {
	return w.aioCreate(nil, opDelete, conn, nil, zeroTime, false)
}

// aioCreate creates a request record, appends it to the pending queue, and
// notifies the watcher loop.
func (w *watcher) aioCreate(ctx any, op OpType, conn net.Conn, buf []byte, deadline time.Time, readfull bool) error {
	if conn == nil {
		return ErrUnsupported
	}
	// Use the concrete connection pointer as its identity in the watcher maps.
	ptr := reflect.ValueOf(conn).Pointer()
	if ptr == 0 {
		return ErrUnsupported
	}

	cb := aiocbPool.Get().(*aiocb)
	*cb = aiocb{op: op, ptr: ptr, size: 0, ctx: ctx, conn: conn, buffer: buf, deadline: deadline, readFull: readfull, idx: -1}

	w.pendingMutex.Lock()
	select {
	case <-w.die:
		w.pendingMutex.Unlock()
		aiocbPool.Put(cb)
		return ErrWatcherClosed
	default:
		w.pendingCreate = append(w.pendingCreate, cb)
	}
	w.pendingMutex.Unlock()

	w.notifyPending()
	return nil
}

// tryRead attempts to read data for pcb. It returns true when the request is
// complete and false when it must wait for another readiness event.
func (w *watcher) tryRead(fd int, pcb *aiocb) bool {
	// Select the request buffer or reserve space in the shared read buffers.
	buf := pcb.buffer

	useSwap := false
	backBuffer := false

	if buf == nil {
		select {
		case <-w.shouldSwap:
			// A swap notification rotates the shared buffers:
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
		default:
		}

		buf = w.swapBufferFront[w.bufferOffset:]
		if len(buf) > 0 {
			useSwap = true
		} else {
			backBuffer = true
			buf = pcb.backBuffer[:]
		}
	}

	// Read until the syscall completes, is interrupted, or would block.
	for {
		nr, er := rawRead(fd, buf[pcb.size:])
		if er == syscall.EAGAIN {
			return false
		}

		// On macOS we can see EINTR here if the user
		// pressed ^Z.
		if er == syscall.EINTR {
			continue
		}

		// Accumulate the number of bytes returned by a successful read.
		if er == nil {
			pcb.size += nr
		}

		pcb.err = er
		// A zero-byte read without an error means the peer closed the stream.
		if nr == 0 && er == nil {
			pcb.err = io.EOF
		}

		break
	}

	// ReadFull uses a caller-provided buffer and remains pending until it is
	// full or an error occurs.
	if pcb.readFull {
		if pcb.err != nil {
			// The operation is complete because it failed.
			return true
		}

		if pcb.size == len(pcb.buffer) {
			// The buffer is full, so the operation is complete.
			return true
		}
		return false
	}

	// Expose the portion of the selected buffer containing the read data.
	if useSwap {
		pcb.useSwap = true
		pcb.buffer = buf[:pcb.size]
		w.bufferOffset += pcb.size
	} else if backBuffer {
		pcb.buffer = buf
	}
	return true
}

// tryWrite attempts to write the remaining data for pcb. It returns true when
// the request is complete and false when it must wait for another readiness event.
func (w *watcher) tryWrite(fd int, pcb *aiocb) bool {
	var nw int
	var ew error

	if pcb.buffer != nil {
		for {
			nw, ew = rawWrite(fd, pcb.buffer[pcb.size:])
			pcb.err = ew

			// The socket cannot currently accept more data; retry after readiness.
			if ew == syscall.EAGAIN {
				return false
			}

			// On macOS and BSD, temporary mbuf exhaustion is also retryable.
			// https://man.freebsd.org/cgi/man.cgi?query=mbuf&sektion=9&format=html
			if ew == syscall.ENOBUFS {
				return false
			}

			// On macOS we can see EINTR here if the user pressed ^Z.
			if ew == syscall.EINTR {
				continue
			}

			// Accumulate the number of bytes written by a successful call.
			if ew == nil {
				pcb.size += nw
			}
			break
		}
	}

	// An incomplete write without an error must wait for the next writable event.
	if pcb.size == len(pcb.buffer) || ew != nil {
		return true
	}

	// The write is incomplete and should be retried later.
	return false
}

// releaseConn removes the connection identified by ident, fails its queued
// requests, and closes its duplicated file descriptor.
func (w *watcher) releaseConn(ident int) {
	if desc, ok := w.descs[ident]; ok {
		// Fail pending reads and remove their deadlines from the timeout heap.
		for e := desc.readers.Front(); e != nil; e = e.Next() {
			tcb := e.Value.(*aiocb)
			// Report that the connection has been released.
			tcb.err = io.ErrClosedPipe
			w.deliver(tcb)
		}

		// Fail pending writes and remove their deadlines from the timeout heap.
		for e := desc.writers.Front(); e != nil; e = e.Next() {
			tcb := e.Value.(*aiocb)
			// Report that the connection has been released.
			tcb.err = io.ErrClosedPipe
			w.deliver(tcb)
		}

		// Remove the connection from the registration maps.
		delete(w.descs, ident)
		delete(w.connIdents, desc.ptr)

		// Reset and return the descriptor record to its pool.
		desc.ptr = 0
		desc.readers.Init()
		desc.writers.Init()
		fdDescPool.Put(desc)

		// Close the duplicate owned by the watcher.
		closeFd(ident)
	}
}

// deliver queues pcb for WaitIO and removes it from the timeout heap, if present.
func (w *watcher) deliver(pcb *aiocb) {
	if pcb.idx != -1 {
		heap.Remove(&w.timeouts, pcb.idx)
		pcb.idx = -1 // mark as removed
	}

	select {
	case w.chResults <- pcb:
	case <-w.die:
	}
}

// loop processes pending requests, poller events, deadlines, and GC notifications.
func (w *watcher) loop() {
	// Release all registered connections when the loop exits.
	defer func() {
		for ident := range w.descs {
			w.releaseConn(ident)
		}
	drainResults:
		for {
			select {
			case pcb := <-w.chResults:
				*pcb = aiocb{}
			default:
				break drainResults
			}
		}
		w.pendingMutex.Lock()
		clear(w.pendingCreate)
		w.pendingCreate = w.pendingCreate[:0]
		clear(w.pendingProcessing)
		w.pendingProcessing = w.pendingProcessing[:0]
		w.pendingMutex.Unlock()
		close(w.loopDone)
	}()

	for {
		select {
		case <-w.chPendingNotify:
			// Swap the submission and processing slices without copying requests.
			w.pendingMutex.Lock()
			w.pendingCreate, w.pendingProcessing = w.pendingProcessing, w.pendingCreate
			clear(w.pendingCreate) // Clear to avoid memory leaks
			w.pendingCreate = w.pendingCreate[:0]
			w.pendingMutex.Unlock()

			// Process this batch before handling another event.
			w.handlePending(w.pendingProcessing)

			// Drop references held by this batch so completed connections can be
			// reclaimed by the garbage collector.
			clear(w.pendingProcessing)
			w.pendingProcessing = w.pendingProcessing[:0]

		case sig := <-w.chSignal: // Process a batch of poller events.
			if sig.err != nil {
				w.pollerErr = sig.err
				w.stop()
				return
			}
			w.handleEvents(sig.events)
			select {
			case sig.done <- struct{}{}:
			case <-w.die:
				return
			}

		case <-w.timer.C: // Expire requests whose deadlines have passed.
			now := time.Now()
			for w.timeouts.Len() > 0 {
				pcb := w.timeouts[0]
				// A delivered request should already have been removed from the heap.
				if pcb.idx == -1 {
					heap.Pop(&w.timeouts)
					continue
				}
				if now.After(pcb.deadline) {
					// Report that the request's deadline expired.
					pcb.err = ErrDeadline
					// Remove the request from its operation queue, if still queued.
					if pcb.l != nil && pcb.elem != nil {
						pcb.l.Remove(pcb.elem)
					}
					// Remove it from the heap before delivering the timeout.
					heap.Pop(&w.timeouts)
					pcb.idx = -1 // mark as removed before deliver to avoid double removal
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

// handleGC releases connections queued by their finalizers.
func (w *watcher) handleGC() {
	w.gcMutex.Lock()
	defer w.gcMutex.Unlock()

	for _, c := range w.gc {
		// Find the registered descriptor using the connection's identity.
		ptr := reflect.ValueOf(c).Pointer()
		if ident, ok := w.connIdents[ptr]; ok {
			w.releaseConn(ident)
		}
		// Keep the connection reachable until its duplicated descriptor is closed.
		runtime.KeepAlive(c)
	}
	w.gcClosed += uint32(len(w.gc))
	// Clear references before reusing the queue.
	clear(w.gc)
	w.gc = w.gc[:0]
}

// handlePending registers connections as needed, then starts or queues requests.
func (w *watcher) handlePending(pending []*aiocb) {
PENDING:
	for _, pcb := range pending {
		ident, ok := w.connIdents[pcb.ptr]
		// Release an already registered connection.
		if pcb.op == opDelete && ok {
			w.releaseConn(ident)
			continue
		}

		// Check if the file descriptor is already registered
		var desc *fdDesc
		if ok {
			desc = w.descs[ident]
		} else {
			// Duplicate and register the socket on its first request.
			dupfd, err := dupconn(pcb.conn)
			if err != nil {
				// Report duplication errors through the normal completion channel.
				pcb.err = err
				w.deliver(pcb)
				continue
			}

			// Register the duplicate before closing the original connection.
			if werr := w.pfd.Watch(dupfd); werr != nil {
				// Close the duplicate if the poller cannot register it.
				closeFd(dupfd)
				pcb.err = werr
				w.deliver(pcb)
				continue
			}

			// The watcher now owns the duplicate, so close the original connection.
			pcb.conn.Close()
			ident = dupfd

			// Reuse a descriptor record and index it by socket and connection.
			desc = fdDescPool.Get().(*fdDesc)
			desc.ptr = pcb.ptr
			desc.readers.Init()
			desc.writers.Init()
			w.descs[ident] = desc
			w.connIdents[pcb.ptr] = ident

			// The finalizer queues an unreachable connection for cleanup. It must
			// not retain the connection itself after the callback returns.
			runtime.SetFinalizer(pcb.conn, func(c net.Conn) {
				w.gcMutex.Lock()
				w.gc = append(w.gc, c)
				w.gcFound++
				w.gcMutex.Unlock()

				// Wake the loop to process the queued connection.
				select {
				case w.gcNotify <- struct{}{}:
				default:
				}
			})
		}

		// The socket is registered; attempt the operation or queue it for readiness.
		switch pcb.op {
		case OpRead:
			// Preserve per-connection ordering by attempting an immediate read
			// only when no earlier read is queued.
			if desc.readers.Len() == 0 {
				if w.tryRead(ident, pcb) {
					w.deliver(pcb)
					// The request completed without being queued.
					continue PENDING
				}
			}

			// Queue the request until the socket becomes readable.
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

		// Track queued requests with deadlines in the timeout heap.
		if !pcb.deadline.IsZero() {
			heap.Push(&w.timeouts, pcb)
			if w.timeouts.Len() > 0 && w.timeouts[0] == pcb {
				w.timer.Reset(time.Until(pcb.deadline))
			}
		}
	}
}

// handleEvents processes a batch of readiness events and retries the queued
// operations for each registered descriptor.
//
// The watcher polls a duplicated descriptor rather than the descriptor owned
// by net.Conn. This keeps events associated with the watched socket even if
// the original descriptor is closed and its number is reused by another
// connection. When the watched duplicate is closed, the platform poller
// removes it; this handler also ignores events for descriptors no longer in
// the registration map.
func (w *watcher) handleEvents(events pollerEvents) {
	for i := range events {
		e := &events[i]
		desc, ok := w.descs[e.ident]
		if !ok {
			continue
		}

		// Retry queued reads when the descriptor is readable.
		if e.ev&EV_READ != 0 && desc.readers.Len() > 0 {
			var next *list.Element
			// Process reads in order until one would block.
			for elem := desc.readers.Front(); elem != nil; elem = next {
				next = elem.Next()
				pcb := elem.Value.(*aiocb)
				if w.tryRead(e.ident, pcb) {
					desc.readers.Remove(elem)
					w.deliver(pcb)
				} else {
					// Later reads must wait behind this request.
					break
				}
			}
		}

		// Retry queued writes when the descriptor is writable.
		if e.ev&EV_WRITE != 0 && desc.writers.Len() > 0 {
			var next *list.Element
			for elem := desc.writers.Front(); elem != nil; elem = next {
				next = elem.Next()
				pcb := elem.Value.(*aiocb)
				if w.tryWrite(e.ident, pcb) {
					desc.writers.Remove(elem)
					w.deliver(pcb)
				} else {
					break
				}
			}
		}
	}
}

// GetGC returns the number of connections queued by finalizers and the number
// of those queued connections processed by the watcher.
func (w *watcher) GetGC() (found uint32, closed uint32) {
	w.gcMutex.Lock()
	defer w.gcMutex.Unlock()
	return w.gcFound, w.gcClosed
}
