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
	"io"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func init() {
	go http.ListenAndServe(":6060", nil)
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
	wbuf := make([]byte, bufsize)
	rbuf := make([]byte, bufsize)
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
						if res.Error != io.EOF {
							log.Println("echoServer read:", res.Error)
						}
						w.Free(res.Conn)
						continue
					}

					if res.Size > 0 {
						copy(wbuf, res.Buffer[:res.Size])
						// write the data, we won't start to read again until write completes.
						// so we only need at most 1 shared read/write buffer for a connection to echo
						w.Write(nil, res.Conn, wbuf[:res.Size])
					}
				case OpWrite:
					if res.Error != nil {
						log.Println("echoServer write:", res.Error)
						w.Free(res.Conn)
						continue
					}

					if res.Size > 0 {
						// write complete, start read again
						w.Read(nil, res.Conn, rbuf)
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

// simulate a server hangup, no read/write
func hangupServer(t testing.TB, bufsize int) net.Listener {
	tcpaddr, _ := net.ResolveTCPAddr("tcp", "localhost:0")
	ln, err := net.ListenTCP("tcp", tcpaddr)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		var conns []net.Conn
		for {
			conn, err := ln.AcceptTCP()
			if err != nil {
				log.Println(err)
				return
			}
			// hold reference to conn only
			conns = append(conns, conn)
		}
	}()
	return ln
}

func TestEchoTiny(t *testing.T) {
	ln := echoServer(t, 1)
	defer ln.Close()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	tx := []byte("hello world")
	rx := make([]byte, len(tx))

	_, err = conn.Write(tx)
	if err != nil {
		t.Fatal(err)
	}

	t.Log("tx:", string(tx))
	_, err = conn.Read(rx)
	if err != nil {
		t.Fatal(err)
	}

	t.Log("rx:", string(tx))
}

func TestDeadline(t *testing.T) {
	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	testSingleDeadline(t, w)
}

func TestEmptyBuffer(t *testing.T) {
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

func TestUnsupportedConn(t *testing.T) {
	ln := echoServer(t, 1)
	defer ln.Close()

	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// nil conn
	if w.Read(nil, nil, nil) != ErrUnsupported {
		t.Fatal("incorrect empty conn handling")
	}

	//
	p1, p2 := net.Pipe()
	w.Write(nil, p1, make([]byte, 1))
	w.Read(nil, p2, make([]byte, 1))

	for {
		results, err := w.WaitIO()
		if err != nil {
			t.Log(err)
		}

		var count int
		for _, res := range results {
			switch res.Operation {
			case OpRead:
				if res.Error == ErrUnsupported {
					count++
					if count == 2 {
						return
					}
				}
			case OpWrite:
				if res.Error == ErrUnsupported {
					count++
					if count == 2 {
						return
					}
				}
			}
		}
	}
}

func testSingleDeadline(t *testing.T, w *Watcher) {
	ln := echoServer(t, 1024)
	defer ln.Close()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	go func() {
		// read with timeout
		err := w.ReadTimeout(nil, conn, nil, time.Now().Add(time.Second))
		if err != nil {
			log.Fatal(err)
		}
	}()

READTEST:
	for {
		results, err := w.WaitIO()
		if err != nil {
			t.Log(err)
			break READTEST
		}

		for _, res := range results {
			switch res.Operation {
			case OpRead:
				if res.Error == ErrDeadline {
					t.Log("read deadline", res.Error)
					break READTEST
				}
			}
		}
	}

	hangup := hangupServer(t, 1024)
	defer hangup.Close()

	buf := make([]byte, 1024*1024)
	go func() {
		// write with timeout
		err = w.WriteTimeout(nil, conn, buf, time.Now().Add(time.Second))
		if err != nil {
			log.Fatal(err)
		}
	}()

WRITETEST:
	for {
		results, err := w.WaitIO()
		if err != nil {
			t.Log(err)
			break WRITETEST
		}

		for _, res := range results {
			switch res.Operation {
			case OpWrite:
				err = w.WriteTimeout(nil, conn, buf, time.Now().Add(time.Second))
				if err != nil {
					log.Fatal(err)
				}

				if res.Error == ErrDeadline {
					t.Log("write deadline", res.Error)
					break WRITETEST
				}
			}
		}
	}

}

func TestEchoHuge(t *testing.T) {
	ln := echoServer(t, 65536)
	defer ln.Close()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	tx := make([]byte, 100*1024*1024)
	rx := make([]byte, len(tx))
	_, err = io.ReadFull(rand.Reader, tx)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		conn.Write(tx)
	}()

	n, err := io.ReadFull(conn, rx)
	if err != nil {
		t.Fatal(err, n)
	}
	t.Log("pong size:", n)

	if !bytes.Equal(tx, rx) {
		t.Fatal("incorrect receiving")
	}
	t.Log("bytes compare successful")
}

func TestBidirectionWatcher(t *testing.T) {
	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	testBidirectionWatcher(t, w)
}

func testBidirectionWatcher(t *testing.T, w *Watcher) {
	ln := echoServer(t, 65536)
	defer ln.Close()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	tx := []byte("hello world")
	go func() {
		// send
		err = w.Write(nil, conn, tx)
		if err != nil {
			log.Fatal(err)
		}

		// read will use internal buffer
		err = w.Read(nil, conn, nil)
		if err != nil {
			log.Fatal(err)
		}
	}()

	for {
		results, err := w.WaitIO()
		if err != nil {
			t.Log(err)
			return
		}

		for _, res := range results {
			switch res.Operation {
			case OpWrite:
				// recv
				if res.Error != nil {
					t.Fatal(res.Error)
				}

				err := w.Read(nil, conn, tx)
				if err != nil {
					t.Fatal(err)
				}
			case OpRead:
				t.Log("read:", res.Error, res.Size)
				return
			}
		}
	}
}

func TestReadFull(t *testing.T) {
	ln := echoServer(t, 65536)
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

	// 100 MB random
	tx := make([]byte, 100*1024*1024)
	rx := make([]byte, len(tx))
	_, err = io.ReadFull(rand.Reader, tx)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		// send
		if err := w.Write(nil, conn, tx); err != nil {
			log.Fatal(err)
		}
	}()

	// submit read full
	err = w.ReadFull(nil, conn, rx, time.Time{})
	if err != nil {
		log.Fatal(err)
	}
	for {
		results, err := w.WaitIO()
		if err != nil {
			t.Log(err)
			return
		}

		for _, res := range results {
			switch res.Operation {
			case OpWrite:
				// recv
				if res.Error != nil {
					t.Fatal(res.Error)
				}
				t.Log("written:", res.Error, res.Size)
			case OpRead:
				t.Log("read:", res.Error, res.Size)
				if res.Size != len(rx) {
					t.Fatal("readfull mismatch", res.Size, len(rx))
				}
				if !bytes.Equal(tx, rx) {
					t.Fatal("readfull content mismatch")
				}
				return
			}
		}
	}
}

func TestSocketClose(t *testing.T) {
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
	defer time.Sleep(1 * time.Second)

	tx := []byte("hello world")
	// send
	err = w.Write(nil, conn, tx)
	if err != nil {
		log.Fatal(err)
	}
	for {
		results, err := w.WaitIO()
		if err != nil {
			t.Log(err)
			return
		}

		for _, res := range results {
			switch res.Operation {
			case OpWrite:
				w.Free(conn)
				w.Read(nil, conn, tx)
			case OpRead:
				if res.Error != nil {
					t.Log(res.Error)
					return
				}
			}
		}
	}
}

func TestWriteOnClosedConn(t *testing.T) {
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

	tx := []byte("hello world")
	go func() {
		// send
		err = w.Write(nil, conn, tx)
		if err != nil {
			log.Fatal(err)
		}
	}()

	for {
		results, err := w.WaitIO()
		if err != nil {
			t.Log(err)
			return
		}

		for _, res := range results {
			switch res.Operation {
			case OpWrite:
				// recv
				if res.Error != nil {
					conn.Close()
					return
				}
			}
		}
	}
}

func Test1k(t *testing.T) {
	testParallel(t, 1024, 1024)
}
func Test2k(t *testing.T) {
	testParallel(t, 2048, 1024)
}

func Test4k(t *testing.T) {
	testParallel(t, 4096, 1024)
}

func Test8k(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to system connection limits")
	}
	testParallel(t, 8192, 1024)
}

func Test10k(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to system connection limits")
	}
	testParallel(t, 10240, 1024)
}

func Test12k(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to system connection limits")
	}
	testParallel(t, 12288, 1024)
}

func Test1kTiny(t *testing.T) {
	testParallel(t, 1024, 16)
}

func Test2kTiny(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to system connection limits")
	}
	testParallel(t, 2048, 16)
}

func Test4kTiny(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to system connection limits")
	}
	testParallel(t, 4096, 16)
}

func testParallel(t *testing.T, par int, msgsize int) {
	t.Log("testing concurrent:", par, "connections")
	ln := echoServer(t, msgsize)
	defer ln.Close()

	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	go func() {
		for i := 0; i < par; i++ {
			data := make([]byte, msgsize)
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				log.Fatal(err)
			}

			// send
			err = w.Write(nil, conn, data)
			if err != nil {
				log.Fatal(err)
			}
		}
	}()

	nbytes := 0
	ntotal := msgsize * par
	for {
		results, err := w.WaitIO()
		if err != nil {
			t.Log(err)
			return
		}

		for _, res := range results {
			switch res.Operation {
			case OpWrite:
				// recv
				if res.Error != nil {
					continue
				}

				err = w.Read(nil, res.Conn, nil)
				if err != nil {
					t.Fatal(err)
				}
			case OpRead:
				if res.Error != nil {
					continue
				}
				if res.Size == 0 {
					continue
				}

				nbytes += res.Size
				if nbytes >= ntotal {
					t.Log("completed:", nbytes)
					return
				}
			}
		}
	}
}

func Test10kRandomSwapBuffer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to system connection limits")
	}
	testParallelRandomInternal(t, 10240, 1024, false)
}

func Test10kCompleteSwapBuffer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to system connection limits")
	}
	testParallelRandomInternal(t, 10240, 1024, true)
}

func testParallelRandomInternal(t *testing.T, par int, msgsize int, allswap bool) {
	t.Log("testing concurrent:", par, "connections")
	ln := echoServer(t, msgsize)
	defer ln.Close()

	w, err := NewWatcherSize(par * msgsize)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	go func() {
		for i := 0; i < par; i++ {
			data := make([]byte, msgsize)
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				log.Fatal(err)
			}

			// send
			err = w.Write(nil, conn, data)
			if err != nil {
				log.Fatal(err)
			}
		}
	}()

	nbytes := 0
	ntotal := msgsize * par
	for {
		results, err := w.WaitIO()
		if err != nil {
			t.Log(err)
			return
		}

		for _, res := range results {
			switch res.Operation {
			case OpWrite:
				// recv
				if res.Error != nil {
					t.Fatal(res.Error)
				}

				// inject random nil buffer to test internal buffer
				var err error
				var rnd int32
				binary.Read(rand.Reader, binary.LittleEndian, &rnd)
				if allswap || rnd%13 == 0 {
					err = w.Read(nil, res.Conn, nil)
				} else {
					err = w.Read(nil, res.Conn, make([]byte, 1024))
				}

				if err != nil {
					t.Fatal(err)
				}
			case OpRead:
				if res.Error != nil {
					continue
				}
				nbytes += res.Size
				if nbytes >= ntotal {
					t.Log("completed:", nbytes)
					return
				}
			}
		}
	}
}

func TestDeadline1k(t *testing.T) {
	testDeadline(t, 1024)
}

func TestDeadline2k(t *testing.T) {
	testDeadline(t, 2048)
}
func TestDeadline4k(t *testing.T) {
	testDeadline(t, 4096)
}

func TestDeadline8k(t *testing.T) {
	testDeadline(t, 8192)
}

func testDeadline(t *testing.T, par int) {
	t.Log("testing concurrent:", par, "unresponsive connections")
	ln := echoServer(t, 128)
	defer ln.Close()

	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	die := make(chan struct{})
	go func() {
		for i := 0; i < par; i++ {
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				log.Fatal(err)
			}
			defer conn.Close()

			// send nothing, but submit a read request with timeout
			err = w.ReadTimeout(nil, conn, nil, time.Now().Add(time.Second))
			if err != nil {
				log.Fatal(err)
			}
		}
		<-die
	}()

	nerrs := 0
	for {
		results, err := w.WaitIO()
		if err != nil {
			t.Log(err)
			return
		}

		for _, res := range results {
			switch res.Operation {
			case OpRead:
				if res.Error == ErrDeadline {
					nerrs++
					if nerrs == par {
						t.Log("all deadline reached")
						close(die)
						return
					}
				}
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
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				return
			}
			if err := w.Write(nil, conn, data); err != nil {
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
	timeout := time.After(10 * time.Second)
	for count < expectedWrites {
		select {
		case <-timeout:
			t.Logf("timeout waiting for results, got %d/%d", count, expectedWrites)
			return count
		default:
		}

		results, err := w.WaitIO()
		if err != nil {
			t.Logf("waitio error: %v", err)
			return count
		}
		// Explicitly clear Conn references to allow GC
		for i := range results {
			results[i].Conn = nil
		}
		count += len(results)
	}
	return count
}

// triggerGCCleanup triggers the GC cleanup by calling WaitIO once more
// to clear the internal results slice, then forces GC.
func triggerGCCleanup(w *Watcher) {
	// Start a goroutine that will block in WaitIO
	// This causes the previous results to be cleared
	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = w.WaitIO()
	}()
	// Wait for the goroutine to start
	<-started
	// Give extra time to ensure WaitIO has cleared w.results
	time.Sleep(200 * time.Millisecond)
}

func TestGC(t *testing.T) {
	par := 200 // Need at least 100 successful GC
	t.Log("testing GC:", par, "connections")
	ln := echoServer(t, 1024)
	defer ln.Close()

	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Create connections and wait for results in a separate function
	// This ensures all local references (results, conn) go out of scope
	count := createConnectionsAndWait(t, w, ln, par)
	t.Logf("received %d results", count)

	// Trigger WaitIO to clear internal results, then force GC
	triggerGCCleanup(w)

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

func TestDoubleClose(t *testing.T) {
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

func TestWaitIOAfterClose(t *testing.T) {
	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	w.Close()

	_, err = w.WaitIO()
	if err != ErrWatcherClosed {
		t.Fatalf("expected ErrWatcherClosed, got %v", err)
	}
}

func TestReadWriteAfterClose(t *testing.T) {
	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	w.Close()

	// Create a dummy connection (doesn't need to be real for this test as we check watcher state first)
	// However, Read/Write checks for nil conn first, so we need something non-nil.
	// But wait, Read/Write checks w.die first? Let's check watcher.go
	// Yes, aioCreate checks w.die first.

	// We need a valid net.Conn interface, but it doesn't need to be connected.
	// But aioCreate checks for nil conn and pointer type.
	// So we can use a closed connection.

	ln, _ := net.Listen("tcp", "localhost:0")
	conn, _ := net.Dial("tcp", ln.Addr().String())
	conn.Close()
	ln.Close()

	if err := w.Read(nil, conn, make([]byte, 1)); err != ErrWatcherClosed {
		t.Fatalf("Read: expected ErrWatcherClosed, got %v", err)
	}

	if err := w.Write(nil, conn, make([]byte, 1)); err != ErrWatcherClosed {
		t.Fatalf("Write: expected ErrWatcherClosed, got %v", err)
	}
}

func TestContextPassing(t *testing.T) {
	ln := echoServer(t, 1024)
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
				if res.Context != ctx {
					t.Fatalf("expected context %v, got %v", ctx, res.Context)
				}
				return
			}
		}
	}
}

func TestSetPollerAffinityError(t *testing.T) {
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

func TestSetLoopAffinityError(t *testing.T) {
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

func TestFree(t *testing.T) {
	ln := echoServer(t, 1024)
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
				done = true
				break
			}
		}
	}

	// Free the connection
	if err := w.Free(conn); err != nil {
		t.Fatal(err)
	}

	// Try to write again, should fail or be ignored (depending on implementation details,
	// but Free should close the fd).
	// Actually, Free submits an opDelete.

	// Let's verify that the connection is indeed closed or at least removed from watcher.
	// Since Free is async, we might need to wait.

	// Wait for potential errors or closure
	// timeout := time.After(1 * time.Second)

	// We can try to read from the connection on the other side (server side), it should get EOF or error.
	// But echoServer handles errors by logging.

	// Let's just check if we can still use the watcher with this conn.
	// Note: Free closes the underlying fd (dupfd), but the net.Conn might still be open?
	// watcher.go: releaseConn closes the dupfd. The original net.Conn was closed in handlePending when registering.
	// So the connection should be fully closed.

	// Let's try to read from conn. It should fail.
	conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 10)
	_, err = conn.Read(buf)
	if err == nil {
		t.Fatal("expected error reading from freed connection")
	}
}
