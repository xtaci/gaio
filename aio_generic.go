package gaio

import (
	"container/list"
	"errors"
	"net"
	"time"
)

const (
	// poller wait max events count
	maxEvents = 4096
	// default internal buffer size
	defaultInternalBufferSize = 65536
)

var (
	// ErrUnsupported means the watcher cannot support this type of connection
	ErrUnsupported = errors.New("unsupported connection type")
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
	// ErrCPUID indicates the given cpuid is invalid
	ErrCPUID = errors.New("no such core")
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

const (
	EV_READ  = 0x1
	EV_WRITE = 0x2
)

// event represent a file descriptor event
type event struct {
	ident int // identifier of this event, usually file descriptor
	ev    int // event mark
}

// events from epoll_wait passing to loop,should be in batch for atomicity.
// and batch processing is the key to amortize context switching costs for
// tiny messages.
type pollerEvents []event

// generic poll struct
type poolGeneric struct {
	cpuid        int32
	cachedEvents []pollerEvents
	cacheIndex   uint
}

func (pg *poolGeneric) initCache(numCache int) {
	pg.cachedEvents = make([]pollerEvents, numCache)
	for k := range pg.cachedEvents {
		pg.cachedEvents[k] = make([]event, 1024)
	}
}

func (pg *poolGeneric) loadCache(size int) pollerEvents {
	pe := pg.cachedEvents[pg.cacheIndex]
	if cap(pe) < size {
		pe = make([]event, 0, 2*size)
		pg.cachedEvents[pg.cacheIndex] = pe
	}
	pe = pe[:0]
	pg.cacheIndex = (pg.cacheIndex + 1) % uint(len(pg.cachedEvents))
	return pe
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

// aiocb contains all info for a single request
type aiocb struct {
	l          *list.List // list where this request belongs to
	elem       *list.Element
	ctx        interface{} // user context associated with this request
	ptr        uintptr     // pointer to conn
	op         OpType      // read or write
	conn       net.Conn    // associated connection for nonblocking-io
	err        error       // error for last operation
	size       int         // size received or sent
	buffer     []byte
	backBuffer [16]byte // small byte buffer used when internal buffer exhausted
	readFull   bool     // requests will read full or error
	useSwap    bool     // mark if the buffer is internal swap buffer
	idx        int      // index for heap op
	deadline   time.Time
}

// Watcher will monitor events and process async-io request(s),
type Watcher struct {
	// a wrapper for watcher for gc purpose
	*watcher
}
