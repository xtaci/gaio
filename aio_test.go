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
						w.Free(res.Conn)
						continue
					}

					if res.Size > 0 {
						// write the data, we won't start to read again until write completes.
						// so we only need at most 1 shared read/write buffer for a connection to echo
						w.Write(nil, res.Conn, res.Buffer[:res.Size])
					}
				case OpWrite:
					if res.Error != nil {
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

				t.Log("written:", res.Error, res.Size)
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
	testParallel(t, 8192, 1024)
}

func Test10k(t *testing.T) {
	testParallel(t, 10240, 1024)
}

func Test12k(t *testing.T) {
	testParallel(t, 12288, 1024)
}

func Test1kTiny(t *testing.T) {
	testParallel(t, 1024, 16)
}

func Test2kTiny(t *testing.T) {
	testParallel(t, 2048, 16)
}

func Test4kTiny(t *testing.T) {
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

				err = w.Read(nil, res.Conn, res.Buffer[:cap(res.Buffer)])
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
	testParallelRandomInternal(t, 10240, 1024, false)
}

func Test10kCompleteSwapBuffer(t *testing.T) {
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

func benchmarkEcho(b *testing.B, bufsize int, numconn int) {
	defer runtime.GC()
	b.Log("benchmark echo with message size:", bufsize, "with", numconn, "parallel connections, for", b.N, "times")
	ln := echoServer(b, bufsize)
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

	count := 0
	target := bufsize * b.N * numconn
	for {
		results, err := w.WaitIO()
		if err != nil {
			b.Fatal("waitio:", err)
			return
		}

		for _, res := range results {
			switch res.Operation {
			case OpWrite:
				if res.Error != nil {
					b.Fatal("read:", res.Error)
				}
				err := w.Write(nil, res.Conn, res.Buffer)
				if err != nil {
					b.Log("wirte:", err)
				}
			case OpRead:
				if res.Error != nil {
					b.Fatal("read:", res.Error)
				}
				count += res.Size
				if count >= target {
					return
				}
				//log.Println("count:", count, "target:", target)
				err := w.Read(nil, res.Conn, res.Buffer)
				if err != nil {
					b.Log("read:", err)
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
