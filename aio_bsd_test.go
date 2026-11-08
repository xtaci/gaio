//go:build darwin || netbsd || freebsd || openbsd || dragonfly

package gaio

import (
	"syscall"
	"testing"
)

// Watch must register the filters before it returns. A registration deferred
// to the poller goroutine can race with the watcher closing the descriptor,
// which can hang close(2) in the macOS kernel.
func TestWatchRegistersFiltersBeforeReturning(t *testing.T) {
	p, err := openPoll()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(p.fd) })

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Close(fds[0])
		syscall.Close(fds[1])
	})

	// The poller goroutine is not running, so only Watch itself can register.
	if err := p.Watch(fds[0]); err != nil {
		t.Fatal(err)
	}

	for _, filter := range []int16{syscall.EVFILT_READ, syscall.EVFILT_WRITE} {
		change := []syscall.Kevent_t{{Ident: uint64(fds[0]), Flags: syscall.EV_DELETE, Filter: filter}}
		if _, err := syscall.Kevent(p.fd, change, nil, nil); err != nil {
			t.Fatalf("filter %d not registered after Watch: %v", filter, err)
		}
	}
}
