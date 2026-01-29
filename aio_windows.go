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

//go:build windows

package gaio

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

var (
	// Windows socket/kernel functions via LazyDLL
	modws2_32               = syscall.NewLazyDLL("ws2_32.dll")
	modkernel32             = syscall.NewLazyDLL("kernel32.dll")
	procWSAPoll             = modws2_32.NewProc("WSAPoll")
	procWSARecv             = modws2_32.NewProc("WSARecv")
	procWSASend             = modws2_32.NewProc("WSASend")
	procWSADuplicateSocketW = modws2_32.NewProc("WSADuplicateSocketW")
	procWSASocketW          = modws2_32.NewProc("WSASocketW")
	procIoctlSocket         = modws2_32.NewProc("ioctlsocket")
	procClosesocket         = modws2_32.NewProc("closesocket")
	procGetCurrentProcessId = modkernel32.NewProc("GetCurrentProcessId")
)

const (
	// WSAPoll events (same as POSIX poll)
	_POLLIN     = 0x0100 // Note: Windows uses different values!
	_POLLPRI    = 0x0200
	_POLLOUT    = 0x0010
	_POLLERR    = 0x0001
	_POLLHUP    = 0x0002
	_POLLNVAL   = 0x0004
	_POLLRDNORM = 0x0100
	_POLLWRNORM = 0x0010

	// WSA error codes
	WSAEWOULDBLOCK = 10035
	WSAEINPROGRESS = 10036
	WSAEINTR       = 10004

	// WSA flags
	WSA_FLAG_OVERLAPPED        = 0x01
	WSA_FLAG_NO_HANDLE_INHERIT = 0x80

	// FIONBIO for ioctlsocket
	FIONBIO = 0x8004667E

	// Invalid socket
	INVALID_SOCKET = uintptr(^uint(0))
)

// WSAPOLLFD structure for WSAPoll (must match Windows layout exactly)
//
//	typedef struct pollfd {
//	  SOCKET  fd;       // 8 bytes on 64-bit, 4 bytes on 32-bit
//	  SHORT   events;   // 2 bytes
//	  SHORT   revents;  // 2 bytes
//	} WSAPOLLFD;
//
// Total size: 12 bytes on 64-bit (no padding), 8 bytes on 32-bit
type wsaPollFd struct {
	fd      uintptr // SOCKET is pointer-sized on Windows
	events  int16
	revents int16
}

// WSAPROTOCOL_INFOW structure for WSADuplicateSocket
type wsaProtocolInfo struct {
	ServiceFlags1     uint32
	ServiceFlags2     uint32
	ServiceFlags3     uint32
	ServiceFlags4     uint32
	ProviderFlags     uint32
	ProviderId        syscall.GUID
	CatalogEntryId    uint32
	ProtocolChain     wsaProtocolChain
	Version           int32
	AddressFamily     int32
	MaxSockAddr       int32
	MinSockAddr       int32
	SocketType        int32
	Protocol          int32
	ProtocolMaxOffset int32
	NetworkByteOrder  int32
	SecurityScheme    int32
	MessageSize       uint32
	ProviderReserved  uint32
	ProtocolName      [256]uint16
}

type wsaProtocolChain struct {
	ChainLen     int32
	ChainEntries [7]uint32
}

// WSABuf for WSARecv/WSASend
type wsaBuf struct {
	Len uint32
	Buf *byte
}

// poller is a WSAPoll-based poller for Windows.
// This is similar to poll() on Unix systems.
type poller struct {
	cpuid int32      // the CPU ID to bind to
	mu    sync.Mutex // mutex to protect fd operations

	// Watched file descriptors
	pollFds    []wsaPollFd     // array for WSAPoll
	fdMap      map[uintptr]int // socket -> index in pollFds
	fdMapMutex sync.RWMutex    // protects fdMap and pollFds

	// Wakeup mechanism using a socket pair
	wakeupReader uintptr
	wakeupWriter uintptr

	// shutdown signal
	die     chan struct{}
	dieOnce sync.Once
}

// closeSocket closes a socket using closesocket()
func closeSocket(fd uintptr) error {
	ret, _, errno := procClosesocket.Call(fd)
	if ret != 0 {
		return errno
	}
	return nil
}

// createSocketPair creates a pair of connected sockets for wakeup signaling.
// On Windows, we use a loopback TCP connection.
func createSocketPair() (reader, writer uintptr, err error) {
	// Create a listener on loopback
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, 0, err
	}
	defer listener.Close()

	addr := listener.Addr().String()

	// Connect to the listener
	connCh := make(chan net.Conn, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			errCh <- err
			return
		}
		connCh <- conn
	}()

	// Accept the connection
	acceptConn, err := listener.Accept()
	if err != nil {
		return 0, 0, err
	}

	// Get the dialed connection
	var dialConn net.Conn
	select {
	case dialConn = <-connCh:
	case err = <-errCh:
		acceptConn.Close()
		return 0, 0, err
	}

	// Extract raw sockets
	var readerFd, writerFd uintptr

	// Reader from accepted connection
	if sc, ok := acceptConn.(interface {
		SyscallConn() (syscall.RawConn, error)
	}); ok {
		rc, _ := sc.SyscallConn()
		rc.Control(func(fd uintptr) {
			// Duplicate to get our own handle
			var err error
			readerFd, err = duplicateSocket(fd)
			if err != nil {
				readerFd = 0
			}
		})
	}
	acceptConn.Close()

	// Writer from dialed connection
	if sc, ok := dialConn.(interface {
		SyscallConn() (syscall.RawConn, error)
	}); ok {
		rc, _ := sc.SyscallConn()
		rc.Control(func(fd uintptr) {
			var err error
			writerFd, err = duplicateSocket(fd)
			if err != nil {
				writerFd = 0
			}
		})
	}
	dialConn.Close()

	if readerFd == 0 || writerFd == 0 {
		if readerFd != 0 {
			closeSocket(readerFd)
		}
		if writerFd != 0 {
			closeSocket(writerFd)
		}
		return 0, 0, errors.New("failed to create socket pair")
	}

	// Set non-blocking mode
	setNonBlocking(readerFd)
	setNonBlocking(writerFd)

	return readerFd, writerFd, nil
}

// duplicateSocket duplicates a socket handle
func duplicateSocket(fd uintptr) (uintptr, error) {
	var protocolInfo wsaProtocolInfo

	pid, _, _ := procGetCurrentProcessId.Call()
	ret, _, errno := procWSADuplicateSocketW.Call(
		fd,
		pid,
		uintptr(unsafe.Pointer(&protocolInfo)),
	)
	if ret != 0 {
		return 0, errno
	}

	r0, _, errno := procWSASocketW.Call(
		uintptr(protocolInfo.AddressFamily),
		uintptr(protocolInfo.SocketType),
		uintptr(protocolInfo.Protocol),
		uintptr(unsafe.Pointer(&protocolInfo)),
		0,
		uintptr(WSA_FLAG_OVERLAPPED|WSA_FLAG_NO_HANDLE_INHERIT),
	)

	if r0 == INVALID_SOCKET {
		return 0, errno
	}

	return r0, nil
}

// setNonBlocking sets a socket to non-blocking mode
func setNonBlocking(fd uintptr) error {
	var mode uint32 = 1
	ret, _, errno := procIoctlSocket.Call(
		fd,
		FIONBIO,
		uintptr(unsafe.Pointer(&mode)),
	)
	if ret != 0 {
		return errno
	}
	return nil
}

func openPoll() (*poller, error) {
	// Create wakeup socket pair
	reader, writer, err := createSocketPair()
	if err != nil {
		return nil, err
	}

	p := &poller{
		cpuid:        -1,
		die:          make(chan struct{}),
		fdMap:        make(map[uintptr]int),
		pollFds:      make([]wsaPollFd, 1, 1024), // Start with wakeup socket
		wakeupReader: reader,
		wakeupWriter: writer,
	}

	// Add wakeup reader to poll set at index 0
	// Use POLLRDNORM for read events on Windows
	p.pollFds[0] = wsaPollFd{
		fd:     reader,
		events: _POLLRDNORM,
	}
	p.fdMap[reader] = 0

	return p, nil
}

// Close shuts down the poller.
func (p *poller) Close() error {
	p.dieOnce.Do(func() {
		close(p.die)
	})
	return p.wakeup()
}

// Watch adds a socket to the poll set for both read and write events.
func (p *poller) Watch(fd int) error {
	p.fdMapMutex.Lock()
	defer p.fdMapMutex.Unlock()

	socket := uintptr(fd)

	// Check if already watching
	if _, exists := p.fdMap[socket]; exists {
		return nil
	}

	// Add to poll set
	// Use POLLRDNORM | POLLWRNORM for read/write events on Windows
	idx := len(p.pollFds)
	p.pollFds = append(p.pollFds, wsaPollFd{
		fd:     socket,
		events: _POLLRDNORM | _POLLWRNORM,
	})
	p.fdMap[socket] = idx

	// Wake up the poller to pick up the new socket
	p.wakeup()

	return nil
}

// unwatch removes a socket from the poll set (internal, called with lock held)
func (p *poller) unwatch(fd uintptr) {
	p.fdMapMutex.Lock()
	defer p.fdMapMutex.Unlock()

	idx, exists := p.fdMap[fd]
	if !exists || idx == 0 { // Don't remove wakeup socket
		return
	}

	// Swap with last element and shrink
	lastIdx := len(p.pollFds) - 1
	if idx != lastIdx {
		p.pollFds[idx] = p.pollFds[lastIdx]
		// Update the map for the moved element
		p.fdMap[p.pollFds[idx].fd] = idx
	}
	p.pollFds = p.pollFds[:lastIdx]
	delete(p.fdMap, fd)
}

// wakeup interrupts WSAPoll by writing to the wakeup socket.
func (p *poller) wakeup() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.wakeupWriter == 0 {
		return ErrPollerClosed
	}

	// Write a single byte to wake up the poller
	buf := []byte{1}
	wsaBuf := wsaBuf{Len: 1, Buf: &buf[0]}
	var sent uint32

	procWSASend.Call(
		p.wakeupWriter,
		uintptr(unsafe.Pointer(&wsaBuf)),
		1,
		uintptr(unsafe.Pointer(&sent)),
		0,
		0,
		0,
	)
	return nil
}

// Wait waits for events using WSAPoll and dispatches them.
func (p *poller) Wait(chSignal chan Signal) {
	eventSet := make(pollerEvents, 0, maxEvents)
	sig := Signal{
		done: make(chan struct{}, 1),
	}

	// Drain buffer for wakeup socket
	drainBuf := make([]byte, 128)

	defer func() {
		p.mu.Lock()
		if p.wakeupReader != 0 {
			closeSocket(p.wakeupReader)
			p.wakeupReader = 0
		}
		if p.wakeupWriter != 0 {
			closeSocket(p.wakeupWriter)
			p.wakeupWriter = 0
		}
		p.mu.Unlock()
	}()

	for {
		select {
		case <-p.die:
			return
		default:
			// Get a snapshot of poll fds
			p.fdMapMutex.RLock()
			pollFdsCopy := make([]wsaPollFd, len(p.pollFds))
			copy(pollFdsCopy, p.pollFds)
			p.fdMapMutex.RUnlock()

			if len(pollFdsCopy) == 0 {
				continue
			}

			// Call WSAPoll with 100ms timeout
			// int WSAPoll(LPWSAPOLLFD fdArray, ULONG fds, INT timeout);
			ret, _, _ := procWSAPoll.Call(
				uintptr(unsafe.Pointer(&pollFdsCopy[0])),
				uintptr(len(pollFdsCopy)),
				uintptr(100), // timeout in milliseconds (INT type)
			)

			// ret is SOCKET_ERROR (-1) on error, 0 on timeout, >0 on events
			nEvents := int32(ret)
			if nEvents <= 0 {
				continue
			}

			// Set affinity if cpuid is set
			if cpuid := atomic.LoadInt32(&p.cpuid); cpuid != -1 {
				setAffinity(cpuid)
				atomic.StoreInt32(&p.cpuid, -1)
			}

			// Process events
			for i := range pollFdsCopy {
				pfd := &pollFdsCopy[i]
				if pfd.revents == 0 {
					continue
				}

				// Handle wakeup socket
				if pfd.fd == p.wakeupReader {
					// Drain the wakeup socket
					for {
						wsaBuf := wsaBuf{Len: uint32(len(drainBuf)), Buf: &drainBuf[0]}
						var received uint32
						var flags uint32
						ret, _, _ := procWSARecv.Call(
							p.wakeupReader,
							uintptr(unsafe.Pointer(&wsaBuf)),
							1,
							uintptr(unsafe.Pointer(&received)),
							uintptr(unsafe.Pointer(&flags)),
							0,
							0,
						)
						if ret != 0 || received == 0 {
							break
						}
					}
					continue
				}

				// Build event for user sockets
				e := event{ident: int(pfd.fd)}

				// Check for read events (POLLRDNORM, POLLHUP, POLLERR)
				if pfd.revents&(_POLLRDNORM|_POLLHUP|_POLLERR) != 0 {
					e.ev |= EV_READ
				}
				// Check for write events (POLLWRNORM, POLLERR)
				if pfd.revents&(_POLLWRNORM|_POLLERR) != 0 {
					e.ev |= EV_WRITE
				}

				if e.ev != 0 {
					eventSet = append(eventSet, e)
				}
			}

			// If we have events, notify the watcher
			if len(eventSet) > 0 {
				sig.events = eventSet

				select {
				case chSignal <- sig:
				case <-p.die:
					return
				}

				select {
				case <-sig.done:
					eventSet = eventSet[:0:cap(eventSet)]
				case <-p.die:
					return
				}
			}
		}
	}
}

// rawRead performs a synchronous non-blocking read for Windows using WSARecv.
func rawRead(fd int, p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}

	wsaBuf := wsaBuf{
		Len: uint32(len(p)),
		Buf: &p[0],
	}

	var received uint32
	var flags uint32

	ret, _, errno := procWSARecv.Call(
		uintptr(fd),
		uintptr(unsafe.Pointer(&wsaBuf)),
		1,
		uintptr(unsafe.Pointer(&received)),
		uintptr(unsafe.Pointer(&flags)),
		0,
		0,
	)

	if ret != 0 {
		if errno == syscall.Errno(WSAEWOULDBLOCK) {
			return -1, syscall.EAGAIN
		}
		return -1, errno
	}
	return int(received), nil
}

// rawWrite performs a synchronous non-blocking write for Windows using WSASend.
func rawWrite(fd int, p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}

	wsaBuf := wsaBuf{
		Len: uint32(len(p)),
		Buf: &p[0],
	}

	var sent uint32

	ret, _, errno := procWSASend.Call(
		uintptr(fd),
		uintptr(unsafe.Pointer(&wsaBuf)),
		1,
		uintptr(unsafe.Pointer(&sent)),
		0,
		0,
		0,
	)

	if ret != 0 {
		if errno == syscall.Errno(WSAEWOULDBLOCK) {
			return -1, syscall.EAGAIN
		}
		return -1, errno
	}
	return int(sent), nil
}

// dupconn duplicates a socket handle on Windows.
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

	var dupHandle uintptr
	var dupErr error

	ec := rc.Control(func(fd uintptr) {
		dupHandle, dupErr = duplicateSocket(fd)
		if dupErr != nil {
			return
		}

		// Set non-blocking mode
		dupErr = setNonBlocking(dupHandle)
		if dupErr != nil {
			closeSocket(dupHandle)
			dupHandle = 0
		}
	})

	if ec != nil {
		return -1, ec
	}
	if dupErr != nil {
		return -1, dupErr
	}

	return int(dupHandle), nil
}

// closeFd closes a socket handle (Windows)
func closeFd(fd int) error {
	return closeSocket(uintptr(fd))
}
