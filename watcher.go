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
)

var (
	ErrNoRawConn     = errors.New("net.Conn does implement net.RawConn")
	ErrWatcherClosed = errors.New("watcher closed")
	ErrBufferedChan  = errors.New("cannot use bufferd chan to notify")
)

// aiocb contains all info for a request
type aiocb struct {
	ctx      interface{}
	fd       int
	buffer   []byte
	internal bool // mark if the buffer is internal
	size     int
	done     chan OpResult
}

// OpResult is the result of an aysnc-io
type OpResult struct {
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
	// A Context along with requests
	Context interface{}
}

// Watcher will monitor events and process async-io request(s),
type Watcher struct {
	pfd *poller // poll fd

	// loop
	chReadableNotify  chan int
	chWritableNotify  chan int
	chStopWatchNotify chan int
	chPendingNotify   chan struct{}

	// lock for pending
	pendingReaders map[int][]aiocb
	pendingWriters map[int][]aiocb
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

	// loop related map
	w.pendingReaders = make(map[int][]aiocb)
	w.pendingWriters = make(map[int][]aiocb)

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

// Read submits an aysnc read requests to be notified via 'done' channel,
//
// the capacity of 'done' has to be be 0, i.e an unbuffered chan.
//
// 'buf' can be set to nil to use internal buffer.
// The sequence of notification can guarantee the buffer will not be overwritten
// before next read notification received via <-done.
func (w *Watcher) Read(fd int, buf []byte, done chan OpResult, ctx interface{}) error {
	if cap(done) != 0 {
		return ErrBufferedChan
	}

	select {
	case <-w.die:
		return ErrWatcherClosed
	default:
		w.pendingMutex.Lock()
		w.pendingReaders[fd] = append(w.pendingReaders[fd], aiocb{ctx: ctx, fd: fd, buffer: buf, done: done})
		w.pendingMutex.Unlock()

		w.notifyPending()
		return nil
	}
}

// Write submits a write requests to be notifed via 'done' channel,
//
// the capacity of 'done' has to be be 0, i.e an unbuffered chan.
//
// the notification order of Write is guaranteed to be sequential.
func (w *Watcher) Write(fd int, buf []byte, done chan OpResult, ctx interface{}) error {
	// do nothing
	if len(buf) == 0 {
		return nil
	}

	if cap(done) != 0 {
		return ErrBufferedChan
	}

	select {
	case <-w.die:
		return ErrWatcherClosed
	default:
		w.pendingMutex.Lock()
		w.pendingWriters[fd] = append(w.pendingWriters[fd], aiocb{ctx: ctx, fd: fd, buffer: buf, done: done})
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
	if pcb.done != nil {
		pcb.done <- OpResult{Fd: pcb.fd, Buffer: buf, Size: nr, Err: er, Context: pcb.ctx}
	}
	return true
}

func (w *Watcher) tryWrite(pcb *aiocb) (complete bool) {
	nw, ew := syscall.Write(pcb.fd, pcb.buffer[pcb.size:])
	if ew == syscall.EAGAIN {
		return false
	}

	// if ew is nil, accumulate bytes written
	if ew == nil {
		pcb.size += nw
	}

	// all bytes written or has error
	if pcb.size == len(pcb.buffer) || ew != nil {
		if pcb.done != nil {
			pcb.done <- OpResult{Fd: pcb.fd, Buffer: pcb.buffer, Size: nw, Err: ew, Context: pcb.ctx}
		}
		return true
	}
	return false
}

// the core event loop of this watcher
func (w *Watcher) loop() {
	queuedReaders := make(map[int][]aiocb)
	queuedWriters := make(map[int][]aiocb)

	// nonblocking queue ops on fd
	tryReadAll := func(fd int) {
		for {
			if len(queuedReaders[fd]) == 0 {
				break
			}

			if w.tryRead(&queuedReaders[fd][0]) {
				queuedReaders[fd] = queuedReaders[fd][1:]
			} else {
				break
			}
		}
	}

	tryWriteAll := func(fd int) {
		for {
			if len(queuedWriters[fd]) == 0 {
				break
			}

			if w.tryWrite(&queuedWriters[fd][0]) {
				queuedWriters[fd] = queuedWriters[fd][1:]
			} else {
				break
			}
		}
	}

	for {
		select {
		case <-w.chPendingNotify:
			w.pendingMutex.Lock()
			rfds := make([]int, 0, len(w.pendingReaders))
			wfds := make([]int, 0, len(w.pendingWriters))
			for fd, cbs := range w.pendingReaders {
				queuedReaders[fd] = append(queuedReaders[fd], cbs...)
				rfds = append(rfds, fd)
				delete(w.pendingReaders, fd)
			}
			for fd, cbs := range w.pendingWriters {
				queuedWriters[fd] = append(queuedWriters[fd], cbs...)
				wfds = append(wfds, fd)
				delete(w.pendingWriters, fd)
			}
			w.pendingMutex.Unlock()

			for _, fd := range rfds {
				tryReadAll(fd)
			}
			for _, fd := range wfds {
				tryWriteAll(fd)
			}
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
