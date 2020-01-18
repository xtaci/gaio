package gaio

import (
	"net"
	"time"
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
// 'buf' can be set to nil to use internal buffer.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
func Read(ctx interface{}, conn net.Conn, buf []byte) error {
	return defaultWatcher.Read(ctx, conn, buf)
}

// ReadTimeout submits an async read request on 'fd' with context 'ctx', using buffer 'buf', and
// expected to be completed before 'deadline', 'buf' can be set to nil to use internal buffer.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged
func ReadTimeout(ctx interface{}, conn net.Conn, buf []byte, deadline time.Time) error {
	return defaultWatcher.ReadTimeout(ctx, conn, buf, deadline)
}

// Write submits an async write request on 'fd' with context 'ctx', using buffer 'buf'.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged
func Write(ctx interface{}, conn net.Conn, buf []byte) error {
	return defaultWatcher.Write(ctx, conn, buf)
}

// WriteTimeout submits an async write request on 'fd' with context 'ctx', using buffer 'buf', and
// expected to be completed before 'deadline'.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
func WriteTimeout(ctx interface{}, conn net.Conn, buf []byte, deadline time.Time) error {
	return defaultWatcher.WriteTimeout(ctx, conn, buf, deadline)
}

// Free let the watcher to release resources related to this conn immediately, like file descriptors
func Free(conn net.Conn) error {
	return defaultWatcher.Free(conn)
}
