package gaio

const (
	// poller wait max events count
	maxEvents = 1024
	// suggested eventQueueSize
	eventQueueSize = 1
)

// event represent a file descriptor event
type event struct {
	ident int  // identifier of this event, usually file descriptor
	r     bool // readable
	w     bool // writable
}

// events from epoll_wait passing to loop,should be in batch for atomicity.
// and batch processing is the key to amortize context switching costs for
// tiny messages.
type pollerEvents []event
