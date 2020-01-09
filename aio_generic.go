package gaio

import "syscall"

// event represent a file descriptor event
type event struct {
	ident int32
	r     bool  // readable
	w     bool  // writable
	err   error // error
}

// id associated connection
type identConn struct {
	ident   int32
	rawconn syscall.RawConn
}
