package gaio

import (
	"net"
	"syscall"
)

// poller wait max events count
const maxEvents = 1024

// event represent a file descriptor event
type event struct {
	ident int  // identifier of this event, usually file descriptor
	r     bool // readable
	w     bool // writable
	err   bool // fd has error
}

// events from epoll_wait passing to loop,should be in batch for atomicity.
// and batch processing is the key to amortize context switching costs for
// tiny messages.
type pollerEvents struct {
	events []event
	done   chan struct{}
}

func newPollerEvents() *pollerEvents {
	pe := new(pollerEvents)
	pe.done = make(chan struct{}, 1)
	return pe
}

// dupconn use RawConn to dup() file descriptor
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
