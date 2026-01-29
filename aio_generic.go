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
	"container/list"
	"errors"
	"net"
	"syscall"
	"time"
)

const (
	// Maximum number of events per poll.
	maxEvents = 4096
	// Default internal buffer size.
	defaultInternalBufferSize = 65536
)

var (
	// ErrUnsupported means the watcher cannot support this type of connection
	ErrUnsupported = errors.New("unsupported connection type")
	// ErrNoRawConn means the connection does not implement SyscallConn
	ErrNoRawConn = errors.New("net.Conn does implement net.RawConn")
	// ErrWatcherClosed means the watcher is closed
	ErrWatcherClosed = errors.New("watcher closed")
	// ErrPollerClosed suggests that the poller has closed
	ErrPollerClosed = errors.New("poller closed")
	// ErrConnClosed means the user called Free() on the related connection
	ErrConnClosed = errors.New("connection closed")
	// ErrDeadline means the operation exceeded its deadline before completion
	ErrDeadline = errors.New("operation exceeded deadline")
	// ErrEmptyBuffer means the buffer is empty
	ErrEmptyBuffer = errors.New("empty buffer")
	// ErrCPUID indicates the given cpuid is invalid
	ErrCPUID = errors.New("no such core")
)

var (
	zeroTime = time.Time{}
)

// OpType defines the operation type.
type OpType int

const (
	// OpRead means the aiocb is a read operation
	OpRead OpType = iota
	// OpWrite means the aiocb is a write operation
	OpWrite
	// Internal operation to delete a related resource
	opDelete
)

const (
	EV_READ  = 0x1
	EV_WRITE = 0x2
)

// event represents a file descriptor event
type event struct {
	ident int  // identifier of this event, usually file descriptor
	ev    int8 // event mark
}

// Events from epoll_wait are passed to the loop in batches for atomicity.
// Batch processing amortizes context-switching costs for tiny messages.
type pollerEvents []event

// Signal packages events; when processing is done, signal on the done channel.
type Signal struct {
	events pollerEvents
	done   chan struct{}
}

// OpResult is the result of an async I/O operation.
type OpResult struct {
	// Operation Type
	Operation OpType
	// User context associated with this request
	Context any
	// Related net.Conn to this result
	Conn net.Conn
	// Buffer points to user's supplied buffer or watcher's internal swap buffer
	Buffer []byte
	// IsSwapBuffer marks true if the buffer is an internal swap buffer
	IsSwapBuffer bool
	// Number of bytes sent or received, Buffer[:Size] is the content sent or received.
	Size int
	// I/O error or timeout error
	Error error
}

// aiocb contains all information for a single request
type aiocb struct {
	l          *list.List // list where this request belongs to
	elem       *list.Element
	ctx        any      // user context associated with this request
	ptr        uintptr  // pointer to conn
	op         OpType   // read or write
	conn       net.Conn // associated connection for non-blocking I/O
	err        error    // error for last operation
	size       int      // size received or sent
	buffer     []byte
	backBuffer [16]byte // per-request small buffer used when internal buffer is exhausted
	readFull   bool     // requests will read full or return an error
	useSwap    bool     // marks whether the buffer is an internal swap buffer
	idx        int      // index for heap ops
	deadline   time.Time
}

// Watcher monitors events and processes async I/O requests.
type Watcher struct {
	// A wrapper for watcher for GC purposes
	*watcher
}

var _zero uintptr

// dupconn uses RawConn to dup() a file descriptor
func dupconn(conn net.Conn) (newfd int, err error) {
	sc, ok := conn.(interface {
		SyscallConn() (syscall.RawConn, error)
	})
	if !ok {
		return -1, ErrUnsupported
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return -1, ErrUnsupported
	}

	// Control() guarantees the integrity of file descriptor
	ec := rc.Control(func(fd uintptr) {
		newfd, err = syscall.Dup(int(fd))
	})

	if ec != nil {
		return -1, ec
	}

	return
}
