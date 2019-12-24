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
	fd     int
	buffer []byte
	size   int
	done   chan OpResult
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
}

// Watcher will monitor events and process async-io request(s),
type Watcher struct {
	pfd *poller // poll fd

	// loop
	chReadableNotify  chan int
	chWritableNotify  chan int
	chStopWatchNotify chan int
	chReaders         chan aiocb
	chWriters         chan aiocb

	// internal buffer for reading
	swapBuffer chan []byte

	die     chan struct{}
	dieOnce sync.Once

	// hold net.Conn to prevent from GC
	conns     map[int]net.Conn
	connsLock sync.Mutex
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
	w.chReaders = make(chan aiocb)
	w.chWriters = make(chan aiocb)

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
	w.connsLock.Lock()
	w.conns[fd] = conn
	w.connsLock.Unlock()
	return fd, nil
}

// StopWatch events related to this fd
func (w *Watcher) StopWatch(fd int) {
	w.pfd.Unwatch(fd)
	w.connsLock.Lock()
	delete(w.conns, fd)
	w.connsLock.Unlock()

	select {
	case w.chStopWatchNotify <- fd:
	case <-w.die:
	}
}

// Read submits a read requests and notify IO-completion with done channel,
// the capacity of done has to be be 0, i.e an unbuffered chan.
func (w *Watcher) Read(fd int, done chan OpResult) error {
	if cap(done) != 0 {
		return ErrBufferedChan
	}
	select {
	case w.chReaders <- aiocb{fd: fd, done: done}:
		return nil
	case <-w.die:
		return ErrWatcherClosed
	}
}

// Write submits a write requests and notify IO-completion with done channel,
// the capacity of done has to be be 0, i.e an unbuffered chan.
func (w *Watcher) Write(fd int, buf []byte, done chan OpResult) error {
	// do nothing
	if len(buf) == 0 {
		return nil
	}

	if cap(done) != 0 {
		return ErrBufferedChan
	}

	select {
	case w.chWriters <- aiocb{fd: fd, buffer: buf, done: done}:
		return nil
	case <-w.die:
		return ErrWatcherClosed
	}
}

// tryRead will try to read data on aiocb and notify
// returns true if io has completed, false means not.
func (w *Watcher) tryRead(pcb *aiocb) (complete bool) {
	buf := <-w.swapBuffer
	defer func() { w.swapBuffer <- buf }()

	nr, er := syscall.Read(pcb.fd, buf)
	if er == syscall.EAGAIN {
		return false
	}
	if pcb.done != nil {
		pcb.done <- OpResult{Fd: pcb.fd, Buffer: buf, Size: nr, Err: er}
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
			pcb.done <- OpResult{Fd: pcb.fd, Buffer: pcb.buffer, Size: nw, Err: ew}
		}
		return true
	}
	return false
}

// the core event loop of this watcher
func (w *Watcher) loop() {
	pendingReaders := make(map[int][]aiocb)
	pendingWriters := make(map[int][]aiocb)

	for {
		select {
		case cb := <-w.chReaders:
			pendingReaders[cb.fd] = append(pendingReaders[cb.fd], cb)

			for {
				if len(pendingReaders[cb.fd]) == 0 {
					break
				}
				if w.tryRead(&pendingReaders[cb.fd][0]) {
					pendingReaders[cb.fd] = pendingReaders[cb.fd][1:]
				} else {
					break
				}
			}
		case cb := <-w.chWriters:
			pendingWriters[cb.fd] = append(pendingWriters[cb.fd], cb)
			for {
				if len(pendingWriters[cb.fd]) == 0 {
					break
				}
				if w.tryWrite(&pendingWriters[cb.fd][0]) {
					pendingWriters[cb.fd] = pendingWriters[cb.fd][1:]
				} else {
					break
				}
			}
		case fd := <-w.chReadableNotify:
			for {
				if len(pendingReaders[fd]) == 0 {
					break
				}

				if w.tryRead(&pendingReaders[fd][0]) {
					pendingReaders[fd] = pendingReaders[fd][1:]
				} else {
					break
				}
			}
		case fd := <-w.chWritableNotify:
			for {
				if len(pendingWriters[fd]) == 0 {
					break
				}

				if w.tryWrite(&pendingWriters[fd][0]) {
					pendingWriters[fd] = pendingWriters[fd][1:]
				} else {
					break
				}
			}
		case fd := <-w.chStopWatchNotify:
			delete(pendingReaders, fd)
			delete(pendingWriters, fd)
		case <-w.die:
			return
		}
	}
}
