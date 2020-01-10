package gaio

// event represent a file descriptor event
type event struct {
	ident int   // identifier of this event, usually file descriptor
	r     bool  // readable
	w     bool  // writable
	err   error // error
}
