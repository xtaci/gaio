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
	"time"
)

const (
	// Maximum number of events per poll.
	maxEvents = 4096
	// Default internal buffer size.
	defaultInternalBufferSize = 65536
)

var (
	// ErrUnsupported indicates that the connection type is not supported.
	ErrUnsupported = errors.New("unsupported connection type")
	// ErrNoRawConn indicates that the connection does not implement SyscallConn.
	ErrNoRawConn = errors.New("net.Conn does implement net.RawConn")
	// ErrWatcherClosed is returned when an operation is submitted to a closed watcher.
	ErrWatcherClosed = errors.New("watcher closed")
	// ErrPollerClosed is returned when an operation is attempted on a closed poller.
	ErrPollerClosed = errors.New("poller closed")
	// ErrConnClosed is returned when SetLoopAffinity is called after shutdown.
	ErrConnClosed = errors.New("connection closed")
	// ErrDeadline indicates that the operation's deadline expired before completion.
	ErrDeadline = errors.New("operation exceeded deadline")
	// ErrEmptyBuffer is returned when an operation requires a non-empty buffer.
	ErrEmptyBuffer = errors.New("empty buffer")
	// ErrCPUID is returned when the requested CPU ID is outside the available range.
	ErrCPUID = errors.New("no such core")
)

var (
	zeroTime = time.Time{}
)

// OpType defines the operation type.
type OpType int

const (
	// OpRead identifies a read operation.
	OpRead OpType = iota
	// OpWrite identifies a write operation.
	OpWrite
	// opDelete is an internal operation that releases a connection.
	opDelete
)

const (
	// EV_READ marks a readiness event for reading.
	EV_READ = 0x1
	// EV_WRITE marks a readiness event for writing.
	EV_WRITE = 0x2
)

// event represents readiness information for a poller identifier.
type event struct {
	ident int  // poller identifier, usually a file descriptor or socket handle
	ev    int8 // combination of EV_READ and EV_WRITE
}

// pollerEvents batches readiness events from the platform poller. The watcher
// acknowledges each batch before polling resumes, preserving batch-wise
// processing while amortizing context-switching costs for small messages.
type pollerEvents []event

// Signal transfers a batch of events to the watcher and receives its completion
// acknowledgment on done.
type Signal struct {
	events pollerEvents
	done   chan struct{}
	err    error
}

// OpResult describes a completed asynchronous I/O operation.
type OpResult struct {
	// Operation is the completed operation's type.
	Operation OpType
	// Context is the value supplied with the request.
	Context any
	// Conn is the connection associated with the request.
	Conn net.Conn
	// Buffer is the request buffer or, for a nil-buffer read, the watcher's
	// internal buffer. Internal buffers remain valid until the next WaitIO call.
	Buffer []byte
	// IsSwapBuffer reports whether Buffer uses the watcher's internal swap buffer.
	IsSwapBuffer bool
	// Size is the number of bytes read or written. For reads, the data is in
	// Buffer[:Size].
	Size int
	// Error is the I/O error, if any, including ErrDeadline.
	Error error
}

// aiocb contains the state for a single asynchronous I/O request.
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

// Watcher monitors readiness events and processes asynchronous I/O requests.
type Watcher struct {
	// The wrapper lets a finalizer close the underlying watcher when it is
	// no longer reachable.
	*watcher
}

var _zero uintptr
