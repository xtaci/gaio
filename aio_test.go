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
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func init() {
	go http.ListenAndServe(":6060", nil)
}

func limitWatcherWait(t *testing.T, w *Watcher, timeout time.Duration) {
	t.Helper()
	timer := time.AfterFunc(timeout, func() { w.Close() })
	t.Cleanup(func() { timer.Stop() })
}

func runWatcherProducer(t *testing.T, w *Watcher, produce func() error) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := produce(); err != nil {
			t.Errorf("submitting operations: %v", err)
			w.Close()
		}
	}()
	t.Cleanup(func() {
		w.Close()
		<-done
	})
}

type echoListener struct {
	w *Watcher
	net.Listener
}

func (eln echoListener) Close() error {
	eln.w.Close()
	err := eln.Listener.Close()

	runtime.GC()
	return err
}

func echoServer(t testing.TB, bufsize int) net.Listener {
	tcpaddr, _ := net.ResolveTCPAddr("tcp", "localhost:0")
	ln, err := net.ListenTCP("tcp", tcpaddr)
	if err != nil {
		t.Fatal(err)
	}

	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}

	w.SetPollerAffinity(0)
	w.SetPollerAffinity(1)

	// ping-pong scheme echo server
	go func() {
		for {
			results, err := w.WaitIO()
			if err != nil {
				log.Println(err)
				return
			}

			for _, res := range results {
				switch res.Operation {
				case OpRead:
					if res.Error != nil {
						if res.Error != io.EOF && !errors.Is(res.Error, syscall.ECONNRESET) {
							log.Println("echoServer read:", res.Error)
						}
						w.Free(res.Conn)
						continue
					}

					if res.Size > 0 {
						// Keep the read buffer alive and unchanged until the async write completes.
						w.Write(nil, res.Conn, res.Buffer[:res.Size])
					}
				case OpWrite:
					if res.Error != nil {
						log.Println("echoServer write:", res.Error)
						w.Free(res.Conn)
						continue
					}

					if res.Size > 0 {
						// write complete, start read again
						w.Read(nil, res.Conn, res.Buffer[:cap(res.Buffer)])
					}
				}
			}
		}
	}()

	go func() {
		for {
			conn, err := ln.AcceptTCP()
			if err != nil {
				log.Println(err)
				w.Close()
				return
			}

			//log.Println("watching", conn.RemoteAddr(), "fd:", fd)

			// kick off
			buf := make([]byte, bufsize)
			err = w.Read(nil, conn, buf)
			if err != nil {
				log.Println(err)
				w.Close()
				return
			}
		}
	}()

	return echoListener{w, ln}
}

func TestEchoServerHandlesSmallPayload(t *testing.T) {
	ln := echoServer(t, 1)
	defer ln.Close()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	tx := []byte("hello world")
	rx := make([]byte, len(tx))

	_, err = conn.Write(tx)
	if err != nil {
		t.Fatal(err)
	}

	t.Log("tx:", string(tx))
	_, err = io.ReadFull(conn, rx)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(tx, rx) {
		t.Fatalf("unexpected echoed data: %q", rx)
	}
	t.Log("rx:", string(rx))
}

func TestReadAndWriteDeadlinesExpire(t *testing.T) {
	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	limitWatcherWait(t, w, 30*time.Second)
	testSingleDeadline(t, w)
}

func TestWriteRejectsEmptyBuffer(t *testing.T) {
	ln := echoServer(t, 1)
	defer ln.Close()

	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if w.Write(nil, conn, nil) != ErrEmptyBuffer {
		t.Fatal("incorrect empty buffer handling in Write")
	}

	if w.WriteTimeout(nil, conn, nil, time.Now().Add(time.Second)) != ErrEmptyBuffer {
		t.Fatal("incorrect empty buffer handling in WriteTimeout")
	}
}

func TestReadWriteRejectUnsupportedConnections(t *testing.T) {
	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	limitWatcherWait(t, w, 30*time.Second)

	// A nil connection is unsupported.
	if w.Read(nil, nil, nil) != ErrUnsupported {
		t.Fatal("incorrect empty conn handling")
	}

	p1, p2 := net.Pipe()
	defer p1.Close()
	defer p2.Close()
	if err := w.Write(nil, p1, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if err := w.Read(nil, p2, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}

	seen := make(map[OpType]bool)
	for len(seen) < 2 {
		results, err := w.WaitIO()
		if err != nil {
			t.Fatal(err)
		}
		for _, res := range results {
			if (res.Operation != OpRead && res.Operation != OpWrite) || res.Error != ErrUnsupported || seen[res.Operation] {
				t.Fatalf("unexpected result: operation=%v error=%v", res.Operation, res.Error)
			}
			seen[res.Operation] = true
		}
	}
}

func testSingleDeadline(t *testing.T, w *Watcher) {
	t.Helper()

	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	conn, err := net.DialTCP("tcp", nil, ln.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := ln.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	peer, err := ln.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if err := conn.SetWriteBuffer(4096); err != nil {
		t.Fatal(err)
	}
	if err := peer.SetReadBuffer(4096); err != nil {
		t.Fatal(err)
	}

	if err := w.ReadTimeout(nil, conn, nil, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	results, err := w.WaitIO()
	if err != nil {
		t.Fatalf("waiting for read deadline: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected one read result, got %d", len(results))
	}
	if res := results[0]; res.Operation != OpRead || res.Conn != conn || res.Error != ErrDeadline {
		t.Fatalf("expected read deadline, got operation=%v conn=%v size=%d error=%v", res.Operation, res.Conn, res.Size, res.Error)
	}

	buf := make([]byte, 1024*1024)
	// The peer never reads, so repeated writes eventually fill its TCP buffers.
	for {
		if err := w.WriteTimeout(nil, conn, buf, time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		results, err := w.WaitIO()
		if err != nil {
			t.Fatalf("waiting for write deadline: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("expected one write result, got %d", len(results))
		}
		res := results[0]
		if res.Operation != OpWrite || res.Conn != conn {
			t.Fatalf("expected write result, got operation=%v conn=%v", res.Operation, res.Conn)
		}
		if res.Error == ErrDeadline {
			if err := w.Free(res.Conn); err != nil {
				t.Fatal(err)
			}
			return
		}
		if res.Error != nil || res.Size != len(buf) {
			t.Fatalf("unexpected write result: size=%d error=%v", res.Size, res.Error)
		}
	}
}

func TestEchoServerHandlesLargePayload(t *testing.T) {
	ln := echoServer(t, 65536)
	defer ln.Close()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(120 * time.Second)); err != nil {
		t.Fatal(err)
	}

	tx := make([]byte, 100*1024*1024)
	rx := make([]byte, len(tx))
	_, err = io.ReadFull(rand.Reader, tx)
	if err != nil {
		t.Fatal(err)
	}

	writeDone := make(chan error, 1)
	go func() {
		n, err := conn.Write(tx)
		if err == nil && n != len(tx) {
			err = io.ErrShortWrite
		}
		writeDone <- err
	}()

	n, err := io.ReadFull(conn, rx)
	if err != nil {
		t.Fatal(err, n)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	t.Log("pong size:", n)

	if !bytes.Equal(tx, rx) {
		t.Fatal("incorrect receiving")
	}
	t.Log("bytes compare successful")
}

func TestWatcherSupportsBidirectionalIO(t *testing.T) {
	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	testBidirectionWatcher(t, w)
}

func testBidirectionWatcher(t *testing.T, w *Watcher) {
	t.Helper()
	limitWatcherWait(t, w, 30*time.Second)
	ln := echoServer(t, 65536)
	defer ln.Close()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	tx := []byte("hello world")
	if err := w.Write(nil, conn, tx); err != nil {
		t.Fatal(err)
	}
	if err := w.Read(nil, conn, nil); err != nil {
		t.Fatal(err)
	}
	var rx []byte
	written := false
	for !written || len(rx) < len(tx) {
		results, err := w.WaitIO()
		if err != nil {
			t.Fatal(err)
		}

		for _, res := range results {
			if res.Conn != conn || res.Error != nil {
				t.Fatalf("unexpected result: operation=%v error=%v", res.Operation, res.Error)
			}
			switch res.Operation {
			case OpWrite:
				if written || res.Size != len(tx) {
					t.Fatalf("unexpected write size: %d", res.Size)
				}
				written = true
			case OpRead:
				rx = append(rx, res.Buffer[:res.Size]...)
				if len(rx) < len(tx) {
					if err := w.Read(nil, conn, nil); err != nil {
						t.Fatal(err)
					}
				} else if !bytes.Equal(rx, tx) {
					t.Fatalf("unexpected echoed data: %q", rx)
				}
			default:
				t.Fatalf("unexpected operation: %v", res.Operation)
			}
		}
	}
	if err := w.Free(conn); err != nil {
		t.Fatal(err)
	}
}

func TestReadFullReceivesRequestedBytes(t *testing.T) {
	ln := echoServer(t, 65536)
	defer ln.Close()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	limitWatcherWait(t, w, 120*time.Second)

	// 100 MB random
	tx := make([]byte, 100*1024*1024)
	rx := make([]byte, len(tx))
	_, err = io.ReadFull(rand.Reader, tx)
	if err != nil {
		t.Fatal(err)
	}

	if err := w.Write(nil, conn, tx); err != nil {
		t.Fatal(err)
	}
	if err := w.ReadFull(nil, conn, rx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	written, read := false, false
	for !written || !read {
		results, err := w.WaitIO()
		if err != nil {
			t.Fatal(err)
		}

		for _, res := range results {
			if res.Conn != conn || res.Error != nil {
				t.Fatalf("unexpected result: operation=%v error=%v", res.Operation, res.Error)
			}
			switch res.Operation {
			case OpWrite:
				if written || res.Size != len(tx) {
					t.Fatalf("unexpected write size: %d", res.Size)
				}
				written = true
			case OpRead:
				if read || res.Size != len(rx) {
					t.Fatalf("readfull mismatch: %d, want %d", res.Size, len(rx))
				}
				if !bytes.Equal(tx, rx) {
					t.Fatal("readfull content mismatch")
				}
				read = true
			default:
				t.Fatalf("unexpected operation: %v", res.Operation)
			}
		}
	}
	if err := w.Free(conn); err != nil {
		t.Fatal(err)
	}
}

func TestFreeClosesConnectionAndSubsequentReadFails(t *testing.T) {
	ln := echoServer(t, 1024)
	defer ln.Close()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	limitWatcherWait(t, w, 30*time.Second)

	tx := []byte("hello world")
	// send
	err = w.Write(nil, conn, tx)
	if err != nil {
		t.Fatal(err)
	}
	for {
		results, err := w.WaitIO()
		if err != nil {
			t.Fatal(err)
		}

		for _, res := range results {
			switch res.Operation {
			case OpWrite:
				if res.Error != nil {
					t.Fatal(res.Error)
				}
				if err := w.Free(conn); err != nil {
					t.Fatal(err)
				}
				if err := w.Read(nil, conn, tx); err != nil {
					t.Fatal(err)
				}
			case OpRead:
				if res.Error == nil {
					t.Fatal("expected read on freed connection to fail")
				}
				return
			default:
				t.Fatalf("unexpected operation: %v", res.Operation)
			}
		}
	}
}

func TestWriteToClosedConnectionReturnsError(t *testing.T) {
	ln := echoServer(t, 1024)
	defer ln.Close()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()

	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	limitWatcherWait(t, w, 30*time.Second)

	tx := []byte("hello world")
	if err := w.Write(nil, conn, tx); err != nil {
		t.Fatal(err)
	}

	for {
		results, err := w.WaitIO()
		if err != nil {
			t.Fatal(err)
		}

		for _, res := range results {
			if res.Operation != OpWrite || res.Conn != conn || res.Error == nil {
				t.Fatalf("expected failed write, got operation=%v error=%v", res.Operation, res.Error)
			}
			return
		}
	}
}

func TestConcurrentEchoWith1024Connections(t *testing.T) {
	testParallel(t, 1024, 1024)
}
func TestConcurrentEchoWith2048Connections(t *testing.T) {
	testParallel(t, 2048, 1024)
}

func TestConcurrentEchoWith4096Connections(t *testing.T) {
	testParallel(t, 4096, 1024)
}

func TestConcurrentEchoWith8192Connections(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to system connection limits")
	}
	if testing.Short() {
		t.Skip("skipping high-concurrency test in short mode")
	}
	testParallel(t, 8192, 1024)
}

func TestConcurrentEchoWith10240Connections(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to system connection limits")
	}
	if testing.Short() {
		t.Skip("skipping high-concurrency test in short mode")
	}
	testParallel(t, 10240, 1024)
}

func TestConcurrentEchoWith12288Connections(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to system connection limits")
	}
	if testing.Short() {
		t.Skip("skipping high-concurrency test in short mode")
	}
	testParallel(t, 12288, 1024)
}

func TestConcurrentEchoWith1024ConnectionsAnd16BytePayload(t *testing.T) {
	testParallel(t, 1024, 16)
}

func TestConcurrentEchoHandlesPartialReads(t *testing.T) {
	testParallel(t, 8, 128*1024)
}

func TestConcurrentEchoHandlesPartialReadsWithMixedBuffers(t *testing.T) {
	testParallelRandomInternal(t, 8, 128*1024, false)
}

func TestConcurrentEchoWith2048ConnectionsAnd16BytePayload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to system connection limits")
	}
	testParallel(t, 2048, 16)
}

func TestConcurrentEchoWith4096ConnectionsAnd16BytePayload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to system connection limits")
	}
	testParallel(t, 4096, 16)
}

func submitEchoWrites(t *testing.T, w *Watcher, addr string, par, msgsize int) {
	t.Helper()
	runWatcherProducer(t, w, func() error {
		for i := 0; i < par; i++ {
			conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
			if err != nil {
				return fmt.Errorf("dial connection %d: %w", i, err)
			}
			if err := conn.(*net.TCPConn).SetLinger(0); err != nil {
				conn.Close()
				return err
			}
			if err := w.Write(nil, conn, make([]byte, msgsize)); err != nil {
				conn.Close()
				return err
			}
		}
		return nil
	})
}

func testParallel(t *testing.T, par int, msgsize int) {
	t.Helper()
	t.Log("testing concurrent:", par, "connections")
	ln := echoServer(t, msgsize)
	defer ln.Close()

	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	limitWatcherWait(t, w, 180*time.Second)
	connBytes := make(map[net.Conn]int, par)

	submitEchoWrites(t, w, ln.Addr().String(), par, msgsize)

	nbytes := 0
	writes, completed := 0, 0
	ntotal := msgsize * par
	for {
		results, err := w.WaitIO()
		if err != nil {
			t.Fatalf("waiting for echo: %d/%d writes, %d/%d connections, %d/%d bytes: %v", writes, par, completed, par, nbytes, ntotal, err)
		}

		for _, res := range results {
			switch res.Operation {
			case OpWrite:
				// recv
				if res.Error != nil {
					if err := w.Free(res.Conn); err != nil {
						t.Fatal(err)
					}
					t.Fatalf("write failed: %v", res.Error)
				}
				writes++

				err = w.Read(nil, res.Conn, nil)
				if err != nil {
					t.Fatal(err)
				}
			case OpRead:
				if res.Error != nil {
					if err := w.Free(res.Conn); err != nil {
						t.Fatal(err)
					}
					t.Fatalf("read failed: %v", res.Error)
				}
				if res.Size <= 0 {
					t.Fatal("empty read result")
				}

				connBytes[res.Conn] += res.Size
				nbytes += res.Size
				if connBytes[res.Conn] > msgsize {
					t.Fatalf("received too many bytes for connection: %d", connBytes[res.Conn])
				}
				if connBytes[res.Conn] == msgsize {
					if err := w.Free(res.Conn); err != nil {
						t.Fatal(err)
					}
					delete(connBytes, res.Conn)
					completed++
				} else if err := w.Read(nil, res.Conn, nil); err != nil {
					t.Fatal(err)
				}
				if nbytes >= ntotal {
					t.Log("completed:", nbytes)
					return
				}
			default:
				t.Fatalf("unexpected operation: %v", res.Operation)
			}
		}
	}
}

func TestConcurrentEchoWithRandomSwapBufferReads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to system connection limits")
	}
	if testing.Short() {
		t.Skip("skipping high-concurrency test in short mode")
	}
	testParallelRandomInternal(t, 10240, 1024, false)
}

func TestConcurrentEchoWithSwapBufferForEveryRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to system connection limits")
	}
	if testing.Short() {
		t.Skip("skipping high-concurrency test in short mode")
	}
	testParallelRandomInternal(t, 10240, 1024, true)
}

func testParallelRandomInternal(t *testing.T, par int, msgsize int, allswap bool) {
	t.Helper()
	t.Log("testing concurrent:", par, "connections")
	ln := echoServer(t, msgsize)
	defer ln.Close()

	w, err := NewWatcherSize(par * msgsize)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	limitWatcherWait(t, w, 180*time.Second)
	connBytes := make(map[net.Conn]int, par)

	submitEchoWrites(t, w, ln.Addr().String(), par, msgsize)

	submitRead := func(conn net.Conn) error {
		var rnd int32
		if err := binary.Read(rand.Reader, binary.LittleEndian, &rnd); err != nil {
			return err
		}
		if allswap || rnd%13 == 0 {
			return w.Read(nil, conn, nil)
		}
		return w.Read(nil, conn, make([]byte, 1024))
	}

	nbytes := 0
	writes, completed := 0, 0
	ntotal := msgsize * par
	for {
		results, err := w.WaitIO()
		if err != nil {
			t.Fatalf("waiting for echo: %d/%d writes, %d/%d connections, %d/%d bytes: %v", writes, par, completed, par, nbytes, ntotal, err)
		}

		for _, res := range results {
			switch res.Operation {
			case OpWrite:
				// recv
				if res.Error != nil {
					if err := w.Free(res.Conn); err != nil {
						t.Fatal(err)
					}
					t.Fatal(res.Error)
				}
				writes++

				if err := submitRead(res.Conn); err != nil {
					t.Fatal(err)
				}
			case OpRead:
				if res.Error != nil {
					if err := w.Free(res.Conn); err != nil {
						t.Fatal(err)
					}
					t.Fatal(res.Error)
				}
				if res.Size <= 0 {
					t.Fatal("empty read result")
				}
				connBytes[res.Conn] += res.Size
				nbytes += res.Size
				if connBytes[res.Conn] > msgsize {
					t.Fatalf("received too many bytes for connection: %d", connBytes[res.Conn])
				}
				if connBytes[res.Conn] == msgsize {
					if err := w.Free(res.Conn); err != nil {
						t.Fatal(err)
					}
					delete(connBytes, res.Conn)
					completed++
				} else if err := submitRead(res.Conn); err != nil {
					t.Fatal(err)
				}
				if nbytes >= ntotal {
					t.Log("completed:", nbytes)
					return
				}
			default:
				t.Fatalf("unexpected operation: %v", res.Operation)
			}
		}
	}
}

func TestReadDeadlineExpiresWith1024Connections(t *testing.T) {
	testDeadline(t, 1024)
}

func TestReadDeadlineExpiresWith2048Connections(t *testing.T) {
	testDeadline(t, 2048)
}
func TestReadDeadlineExpiresWith4096Connections(t *testing.T) {
	testDeadline(t, 4096)
}

func TestReadDeadlineExpiresWith8192Connections(t *testing.T) {
	testDeadline(t, 8192)
}

func testDeadline(t *testing.T, par int) {
	t.Helper()
	t.Log("testing concurrent:", par, "unresponsive connections")
	ln := echoServer(t, 128)
	defer ln.Close()

	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	limitWatcherWait(t, w, 180*time.Second)

	runWatcherProducer(t, w, func() error {
		for i := 0; i < par; i++ {
			conn, err := net.DialTimeout("tcp", ln.Addr().String(), 10*time.Second)
			if err != nil {
				return fmt.Errorf("dial connection %d: %w", i, err)
			}
			if err := conn.(*net.TCPConn).SetLinger(0); err != nil {
				conn.Close()
				return err
			}
			// send nothing, but submit a read request with timeout
			err = w.ReadTimeout(nil, conn, nil, time.Now().Add(time.Second))
			if err != nil {
				conn.Close()
				return err
			}
		}
		return nil
	})

	nerrs := 0
	for {
		results, err := w.WaitIO()
		if err != nil {
			t.Fatal(err)
		}

		for _, res := range results {
			if res.Operation != OpRead || res.Error != ErrDeadline {
				t.Fatalf("expected read deadline, got operation=%v error=%v", res.Operation, res.Error)
			}
			nerrs++
			if err := w.Free(res.Conn); err != nil {
				t.Fatal(err)
			}
			if nerrs == par {
				t.Log("all deadlines reached")
				return
			}
		}
	}
}

func BenchmarkEcho128B(b *testing.B) {
	benchmarkEcho(b, 128, 1)
}

func BenchmarkEcho1K(b *testing.B) {
	benchmarkEcho(b, 1024, 1)
}

func BenchmarkEcho4K(b *testing.B) {
	benchmarkEcho(b, 4096, 1)
}

func BenchmarkEcho64K(b *testing.B) {
	benchmarkEcho(b, 65536, 1)
}

func BenchmarkEcho128K(b *testing.B) {
	benchmarkEcho(b, 128*1024, 1)
}

func BenchmarkEcho128BParallel(b *testing.B) {
	benchmarkEcho(b, 128, 128)
}

func BenchmarkEcho1KParallel(b *testing.B) {
	benchmarkEcho(b, 1024, 128)
}

func BenchmarkEcho4KParallel(b *testing.B) {
	benchmarkEcho(b, 4096, 128)
}

func BenchmarkEcho64KParallel(b *testing.B) {
	benchmarkEcho(b, 65536, 128)
}

func BenchmarkEcho128KParallel(b *testing.B) {
	benchmarkEcho(b, 128*1024, 128)
}

func BenchmarkEcho4KParallel1024(b *testing.B) {
	benchmarkEcho(b, 4096, 1024)
}

func BenchmarkEcho16KParallel1024(b *testing.B) {
	benchmarkEcho(b, 4096, 1024)
}

func benchmarkEcho(b *testing.B, bufsize int, numconn int) {
	defer runtime.GC()
	b.Log("benchmark echo with message size:", bufsize, "with", numconn, "parallel connections, for", b.N, "times")
	ln := echoServer(b, bufsize*numconn)
	defer ln.Close()

	w, err := NewWatcher()
	if err != nil {
		b.Fatal("new watcher:", err)
	}
	defer w.Close()

	addr, _ := net.ResolveTCPAddr("tcp", ln.Addr().String())
	tx := make([]byte, bufsize)
	for i := 0; i < numconn; i++ {
		rx := make([]byte, bufsize)
		conn, err := net.DialTCP("tcp", nil, addr)
		if err != nil {
			b.Fatal("dial:", err)
			return
		}
		w.Write(nil, conn, tx)
		w.Read(nil, conn, rx)
		defer conn.Close()
	}

	b.ReportAllocs()
	b.SetBytes(int64(bufsize * numconn))
	b.ResetTimer()

	target := bufsize * b.N * numconn
	numWritten := bufsize
	numRead := 0

	for {
		results, err := w.WaitIO()
		if err != nil {
			b.Fatal("waitio:", err)
			return
		}

		for _, res := range results {
			switch res.Operation {
			case OpWrite:
				if numWritten < target {
					if res.Error == nil {
						numWritten += bufsize
					} else {
						b.Log("write:", res.Error)
					}

					err := w.Write(nil, res.Conn, res.Buffer)
					if err != nil {
						b.Log("write:", err)
					}
				}

			case OpRead:
				if res.Error != nil {
					b.Fatal("read:", res.Error)
				}
				//log.Println("count:", count, "target:", target)
				err := w.Read(nil, res.Conn, res.Buffer)
				if err != nil {
					b.Log("read:", err)
				}

				numRead += bufsize
				if numRead >= target {
					return
				}
			}
		}
	}
}

func BenchmarkContextSwitch(b *testing.B) {
	die := make(chan struct{})
	ch := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
				ch <- struct{}{}
			case <-die:
				return
			}
		}
	}()

	for i := 0; i < b.N; i++ {
		ch <- struct{}{}
		<-ch
	}
	close(die)
}

// createConnectionsAndWait creates connections, sends data, and waits for results.
// Keeping this in a separate function ensures all local references go out of scope.
func createConnectionsAndWait(t *testing.T, w *Watcher, ln net.Listener, par int) int {
	msgsize := 1024
	var successCount int32
	var wg sync.WaitGroup

	for i := 0; i < par; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data := make([]byte, msgsize)
			conn, err := net.DialTimeout("tcp", ln.Addr().String(), 10*time.Second)
			if err != nil {
				t.Errorf("dial: %v", err)
				return
			}
			if err := w.Write(nil, conn, data); err != nil {
				conn.Close()
				t.Errorf("submit write: %v", err)
				return
			}
			atomic.AddInt32(&successCount, 1)
		}()
	}

	// Wait for all goroutines to submit their writes
	wg.Wait()

	expectedWrites := int(atomic.LoadInt32(&successCount))
	t.Logf("expected writes: %d", expectedWrites)

	count := 0
	for count < expectedWrites {
		results, err := w.WaitIO()
		if err != nil {
			t.Fatalf("waiting for results, got %d/%d: %v", count, expectedWrites, err)
		}
		// Explicitly clear Conn references to allow GC
		for i := range results {
			if results[i].Operation != OpWrite || results[i].Error != nil {
				t.Fatalf("unexpected result: operation=%v error=%v", results[i].Operation, results[i].Error)
			}
			results[i].Conn = nil
		}
		count += len(results)
	}
	return count
}

func TestFinalizerClosesUnreachableConnections(t *testing.T) {
	par := 200 // Need at least 100 successful GC
	t.Log("testing GC:", par, "connections")
	ln := echoServer(t, 1024)
	defer ln.Close()

	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	limitWatcherWait(t, w, 30*time.Second)

	// Create connections and wait for results in a separate function
	// This ensures all local references (results, conn) go out of scope
	count := createConnectionsAndWait(t, w, ln, par)
	t.Logf("received %d results", count)

	// Force more aggressive GC by allocating and discarding memory
	// This helps ensure finalizers are processed
	var found, closed uint32
	for attempt := 0; attempt < 20; attempt++ {
		// Allocate memory to trigger GC pressure
		for i := 0; i < 1000; i++ {
			_ = make([]byte, 10000)
		}
		runtime.GC()
		runtime.Gosched() // Allow finalizer goroutine to run
		time.Sleep(50 * time.Millisecond)

		found, closed = w.GetGC()
		t.Logf("attempt %d: GC found:%d closed:%d", attempt+1, found, closed)

		// Need at least 100 successful GC operations
		if found >= 100 && found == closed {
			t.Logf("GC test passed: found=%d closed=%d", found, closed)
			return
		}
	}

	// Final check
	if found < 100 {
		t.Fatalf("GC found too few: found=%d closed=%d, need at least 100", found, closed)
	}
	if found != closed {
		t.Fatalf("GC mismatch: found=%d closed=%d", found, closed)
	}
	t.Logf("GC test completed: found=%d closed=%d", found, closed)
}

func TestCloseIsIdempotent(t *testing.T) {
	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}

	// First close
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Second close should be safe
	if err := w.Close(); err != nil {
		t.Fatal("second close failed:", err)
	}
}

func TestWaitIOReturnsClosedErrorAfterClose(t *testing.T) {
	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = w.WaitIO()
	if err != ErrWatcherClosed {
		t.Fatalf("expected ErrWatcherClosed, got %v", err)
	}
}

func TestCloseCancelsPendingRead(t *testing.T) {
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := ln.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	serverConn, err := ln.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer serverConn.Close()

	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Read(nil, serverConn, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.WaitIO(); err != ErrWatcherClosed {
		t.Fatalf("expected ErrWatcherClosed after canceling pending read, got %v", err)
	}
}

func TestWaitIOReturnsPollerError(t *testing.T) {
	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	limitWatcherWait(t, w, 30*time.Second)

	pollerErr := errors.New("poller failed")
	w.chSignal <- Signal{err: pollerErr}

	if _, err := w.WaitIO(); !errors.Is(err, pollerErr) {
		t.Fatalf("expected poller error %v, got %v", pollerErr, err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReadAndWriteReturnClosedErrorAfterClose(t *testing.T) {
	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Use a non-nil connection; the closed-watcher check runs before connection validation.
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()

	if err := w.Read(nil, conn, make([]byte, 1)); err != ErrWatcherClosed {
		t.Fatalf("Read: expected ErrWatcherClosed, got %v", err)
	}

	if err := w.Write(nil, conn, make([]byte, 1)); err != ErrWatcherClosed {
		t.Fatalf("Write: expected ErrWatcherClosed, got %v", err)
	}
}

func TestWriteResultPreservesContext(t *testing.T) {
	ln := echoServer(t, 1024)
	defer ln.Close()

	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	limitWatcherWait(t, w, 30*time.Second)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx := "my-context"
	tx := []byte("hello")

	// Write with context
	if err := w.Write(ctx, conn, tx); err != nil {
		t.Fatal(err)
	}

	// Wait for result
	for {
		results, err := w.WaitIO()
		if err != nil {
			t.Fatal(err)
		}

		for _, res := range results {
			if res.Operation == OpWrite {
				if res.Error != nil {
					t.Fatal(res.Error)
				}
				if res.Context != ctx {
					t.Fatalf("expected context %v, got %v", ctx, res.Context)
				}
				if err := w.Free(res.Conn); err != nil {
					t.Fatal(err)
				}
				return
			}
			t.Fatalf("unexpected operation: %v", res.Operation)
		}
	}
}

func TestSetPollerAffinityRejectsInvalidCPUIDs(t *testing.T) {
	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Invalid CPU ID (negative)
	if err := w.SetPollerAffinity(-1); err != ErrCPUID {
		t.Fatalf("expected ErrCPUID, got %v", err)
	}

	// Invalid CPU ID (too large)
	if err := w.SetPollerAffinity(runtime.NumCPU() + 100); err != ErrCPUID {
		t.Fatalf("expected ErrCPUID, got %v", err)
	}
}

func TestSetLoopAffinityRejectsInvalidCPUIDs(t *testing.T) {
	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Invalid CPU ID (negative)
	if err := w.SetLoopAffinity(-1); err != ErrCPUID {
		t.Fatalf("expected ErrCPUID, got %v", err)
	}

	// Invalid CPU ID (too large)
	if err := w.SetLoopAffinity(runtime.NumCPU() + 100); err != ErrCPUID {
		t.Fatalf("expected ErrCPUID, got %v", err)
	}
}

func TestFreeClosesDelegatedConnection(t *testing.T) {
	ln := echoServer(t, 1024)
	defer ln.Close()

	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	limitWatcherWait(t, w, 30*time.Second)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	// Send some data to ensure connection is registered
	if err := w.Write(nil, conn, []byte("hello")); err != nil {
		t.Fatal(err)
	}

	// Wait for write to complete
	done := false
	for !done {
		results, err := w.WaitIO()
		if err != nil {
			t.Fatal(err)
		}
		for _, res := range results {
			if res.Operation == OpWrite {
				if res.Error != nil {
					t.Fatal(res.Error)
				}
				done = true
				break
			}
			t.Fatalf("unexpected operation: %v", res.Operation)
		}
	}

	// Free the connection
	if err := w.Free(conn); err != nil {
		t.Fatal(err)
	}

	// Registration already closed the original net.Conn, so this checks that
	// delegated connections cannot be used directly.
	conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 10)
	_, err = conn.Read(buf)
	if err == nil {
		t.Fatal("expected error reading from freed connection")
	}
}
