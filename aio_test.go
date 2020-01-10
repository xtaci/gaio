package gaio

import (
	"bytes"
	"crypto/rand"
	"io"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"testing"
	"time"
)

func init() {
	go http.ListenAndServe(":6060", nil)
}

func echoServer(t testing.TB, bufsize int) net.Listener {
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}

	w, err := NewWatcher(bufsize)
	if err != nil {
		t.Fatal(err)
	}

	// ping-pong scheme echo server
	go func() {
		wbuffers := make(map[net.Conn][]byte)
		for {
			res, err := w.WaitIO()
			if err != nil {
				log.Println(err)
				return
			}

			switch res.Op {
			case OpRead:
				if res.Err != nil {
					log.Println("read error:", res.Err, res.Size)
					delete(wbuffers, res.Conn)
					res.Conn.Close()
					continue
				}

				if res.Size == 0 {
					delete(wbuffers, res.Conn)
					res.Conn.Close()
					continue
				}

				// write the data, we won't start to read again until write completes.
				// so we only need at most 1 write buffer for a connection
				buf, ok := wbuffers[res.Conn]
				if !ok {
					buf = make([]byte, bufsize)
					wbuffers[res.Conn] = buf
				}
				copy(buf, res.Buffer[:res.Size])
				w.Write(nil, res.Conn, buf[:res.Size])
			case OpWrite:
				if res.Err != nil {
					log.Println("write error:", res.Err, res.Size)
					delete(wbuffers, res.Conn)
					res.Conn.Close()
					continue
				}
				// write complete, start read again
				w.Read(nil, res.Conn, nil)
			}
		}
	}()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				w.Close()
				return
			}

			//log.Println("watching", conn.RemoteAddr(), "fd:", fd)

			// kick off
			err = w.Read(nil, conn, nil)
			if err != nil {
				log.Println(err)
				w.Close()
				return
			}
		}
	}()
	return ln
}

func TestEchoTiny(t *testing.T) {
	ln := echoServer(t, 1024)
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
	conn.Close()
}

func TestDeadline(t *testing.T) {
	ln := echoServer(t, 1024)
	defer ln.Close()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	w, err := NewWatcher(1024)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		// read with timeout
		err = w.ReadTimeout(nil, conn, nil, time.Now().Add(time.Second))
		if err != nil {
			log.Fatal(err)
		}
	}()

	for {
		res, err := w.WaitIO()
		if err != nil {
			t.Log(err)
			return
		}

		switch res.Op {
		case OpRead:
			if res.Err != ErrDeadline {
				t.Fatal(res.Err, "mismatch")
				conn.Close()
			}
			return
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
	defer conn.Close()

	tx := make([]byte, 100*1024*1024)
	n, err := io.ReadFull(rand.Reader, tx)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		n, err := conn.Write(tx)
		if err != nil {
			log.Fatal(err)
		}
		log.Println("ping size", n)
	}()

	rx := make([]byte, len(tx))
	n, err = io.ReadFull(conn, rx)
	if err != nil {
		t.Fatal(err, n)
	}
	t.Log("pong size:", n)

	if !bytes.Equal(tx, rx) {
		t.Fatal("incorrect receiving")
	}
	t.Log("bytes compare successful")
	conn.Close()
}

func TestBidirectionWatcher(t *testing.T) {
	ln := echoServer(t, 65536)
	defer ln.Close()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	w, err := NewWatcher(65536)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	defer time.Sleep(2 * time.Second)

	tx := []byte("hello world")
	go func() {
		// send
		err = w.Write(nil, conn, tx)
		if err != nil {
			log.Fatal(err)
		}
	}()

	for {
		res, err := w.WaitIO()
		if err != nil {
			t.Log(err)
			return
		}

		switch res.Op {
		case OpWrite:
			// recv
			if res.Err != nil {
				t.Fatal(res.Err)
			}

			t.Log("written:", res.Err, res.Size)
			err := w.Read(nil, conn, nil)
			if err != nil {
				t.Fatal(err)
			}
		case OpRead:
			t.Log("read:", res.Err, res.Size)
			conn.Close()
			return
		}
	}
}

func TestWriteOnClosedConn(t *testing.T) {
	ln := echoServer(t, 65536)
	defer ln.Close()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()

	w, err := NewWatcher(65536)
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
		res, err := w.WaitIO()
		if err != nil {
			t.Log(err)
			return
		}

		switch res.Op {
		case OpWrite:
			// recv
			if res.Err != nil {
				conn.Close()
				return
			}
		}
	}
}

func Test1k(t *testing.T) {
	testParallel(t, 1024)
}
func Test2k(t *testing.T) {
	testParallel(t, 2048)
}

func Test4k(t *testing.T) {
	testParallel(t, 4096)
}

/*
func Test8k(t *testing.T) {
	testParallel(t, 8192)
}
*/

func testParallel(t *testing.T, par int) {
	t.Log("testing concurrent:", par, "connections")
	ln := echoServer(t, 1024)
	defer ln.Close()

	w, err := NewWatcher(1024)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	data := make([]byte, 1024)

	die := make(chan struct{})
	go func() {
		for i := 0; i < par; i++ {
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				log.Fatal(err)
			}
			defer conn.Close()

			// send
			err = w.Write(nil, conn, data)
			if err != nil {
				log.Fatal(err)
			}
		}
		<-die
	}()

	nbytes := 0
	ntotal := len(data) * par
	for {
		res, err := w.WaitIO()
		if err != nil {
			t.Log(err)
			return
		}

		switch res.Op {
		case OpWrite:
			// recv
			if res.Err != nil {
				res.Conn.Close()
				continue
			}

			err := w.Read(nil, res.Conn, nil)
			if err != nil {
				t.Fatal(err)
			}
		case OpRead:
			if res.Err != nil {
				res.Conn.Close()
				continue
			}
			if res.Size == 0 {
				res.Conn.Close()
				continue
			}

			nbytes += res.Size
			if nbytes >= ntotal {
				t.Log("completed:", nbytes)
				close(die)
				return
			}
		}
	}
}

func TestDeadline1k(t *testing.T) {
	testDeadline(t, 1024)
}

/* travis cannot authorize enough fd
func TestDeadline2k(t *testing.T) {
	testDeadline(t, 2048)
}
func TestDeadline4k(t *testing.T) {
	testDeadline(t, 4096)
}

func TestDeadline8k(t *testing.T) {
	testDeadline(t, 8192)
}
*/

func testDeadline(t *testing.T, par int) {
	t.Log("testing concurrent:", par, "unresponsive connections")
	ln := echoServer(t, 1024)
	defer ln.Close()

	w, err := NewWatcher(1024)
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
		res, err := w.WaitIO()
		if err != nil {
			t.Log(err)
			return
		}

		switch res.Op {
		case OpRead:
			if res.Err == ErrDeadline {
				res.Conn.Close()
				nerrs++
				if nerrs == par {
					t.Log("all deadline reached")
					close(die)
					return
				}
			}

			if res.Err != nil {
				res.Conn.Close()
			}

			if res.Size == 0 {
				res.Conn.Close()
			}
		}
	}
}

func BenchmarkEcho4K(b *testing.B) {
	benchmarkEcho(b, 4096)
}

func BenchmarkEcho64K(b *testing.B) {
	benchmarkEcho(b, 65536)
}

func BenchmarkEcho128K(b *testing.B) {
	benchmarkEcho(b, 128*1024)
}

func benchmarkEcho(b *testing.B, bufsize int) {
	ln := echoServer(b, bufsize)
	defer ln.Close()

	numLoops := b.N
	addr, _ := net.ResolveTCPAddr("tcp", ln.Addr().String())
	tx := make([]byte, 1024*1024)
	_, err := io.ReadFull(rand.Reader, tx)
	if err != nil {
		b.Fatal(err)
	}

	rx := make([]byte, bufsize)

	conn, err := net.DialTCP("tcp", nil, addr)
	if err != nil {
		b.Fatal(err)
		return
	}
	defer conn.Close()

	b.Log("sending", len(tx), "bytes for", b.N, "times", "with buffer size:", bufsize)
	b.ReportAllocs()
	b.SetBytes(int64(len(tx)))
	b.ResetTimer()
	go func() {
		for i := 0; i < numLoops; i++ {
			_, err := conn.Write(tx)
			if err != nil {
				log.Fatal(err)
			}
		}
	}()

	count := 0
	for {
		n, err := conn.Read(rx)
		if err != nil {
			b.Fatal(err)
		}
		count += n
		if count == len(tx)*numLoops {
			break
		}
	}
}
