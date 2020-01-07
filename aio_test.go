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

	w, err := CreateWatcher(bufsize)
	if err != nil {
		t.Fatal(err)
	}

	// ping-pong scheme echo server
	go func() {
		wbuffers := make(map[int][]byte)
		for {
			res, err := w.WaitIO()
			if err != nil {
				t.Log(err)
				return
			}

			switch res.Op {
			case OpRead:
				if res.Err != nil {
					log.Println("read error:", res.Err, res.Size)
					delete(wbuffers, res.Fd)
					w.CloseConn(res.Fd)
					continue
				}

				if res.Size == 0 {
					delete(wbuffers, res.Fd)
					w.CloseConn(res.Fd)
					continue
				}

				// write the data, we won't start to read again until write completes.
				// so we only need at most 1 write buffer for a connection
				buf, ok := wbuffers[res.Fd]
				if !ok {
					buf = make([]byte, bufsize)
					wbuffers[res.Fd] = buf
				}
				copy(buf, res.Buffer[:res.Size])
				w.Write(nil, res.Fd, buf[:res.Size])
			case OpWrite:
				if res.Err != nil {
					log.Println("write error:", res.Err, res.Size)
					delete(wbuffers, res.Fd)
					w.CloseConn(res.Fd)
				}
				// write complete, start read again
				w.Read(nil, res.Fd, nil)
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

			fd, err := w.NewConn(conn)
			if err != nil {
				w.Close()
				return
			}

			//log.Println("watching", conn.RemoteAddr(), "fd:", fd)

			// kick off
			err = w.Read(nil, fd, nil)
			if err != nil {
				w.Close()
				log.Println(err)
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

	w, err := CreateWatcher(1024)
	if err != nil {
		t.Fatal(err)
	}

	fd, err := w.NewConn(conn)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		// read with timeout
		err = w.ReadTimeout(nil, fd, nil, time.Now().Add(time.Second))
		if err != nil {
			t.Fatal(err)
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
	tx := make([]byte, 100*1024*1024)
	n, err := io.ReadFull(rand.Reader, tx)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		n, err := conn.Write(tx)
		if err != nil {
			t.Fatal(err)
		}
		t.Log("ping size", n)
	}()

	rx := make([]byte, len(tx))
	n, err = io.ReadFull(conn, rx)
	if err != nil {
		t.Fatal(err, n)
	}
	t.Log("pong size:", n)

	if bytes.Compare(tx, rx) != 0 {
		t.Fatal("incorrect receiving")
	}
	conn.Close()
}

func TestBidirectionWatcher(t *testing.T) {
	ln := echoServer(t, 65536)
	defer ln.Close()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	w, err := CreateWatcher(65536)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	fd, err := w.NewConn(conn)
	if err != nil {
		t.Fatal(err)
	}

	tx := []byte("hello world")
	go func() {
		// send
		err = w.Write(nil, fd, tx)
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
			err := w.Read(nil, fd, nil)
			if err != nil {
				t.Fatal(err)
			}
		case OpRead:
			t.Log("read:", res.Err, res.Size)
			return
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

func Test8k(t *testing.T) {
	testParallel(t, 8192)
}

func testParallel(t *testing.T, par int) {
	ln := echoServer(t, 1024)
	defer ln.Close()

	w, err := CreateWatcher(1024)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	data := make([]byte, 1024)

	go func() {
		for i := 0; i < par; i++ {
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}

			fd, err := w.NewConn(conn)
			if err != nil {
				t.Fatal(err)
			}
			// send
			err = w.Write(nil, fd, data)
			if err != nil {
				t.Fatal(err)
			}
		}
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
				t.Fatal(res.Err)
			}

			err := w.Read(nil, res.Fd, nil)
			if err != nil {
				t.Fatal(err)
			}
		case OpRead:
			if res.Err != nil {
				t.Fatal(err)
			}
			nbytes += res.Size
			if nbytes >= ntotal {
				t.Log("completed:", nbytes)
				return
			}
		}
	}
}

func BenchmarkEcho(b *testing.B) {
	ln := echoServer(b, 65536)
	defer ln.Close()

	numLoops := b.N
	addr, _ := net.ResolveTCPAddr("tcp", ln.Addr().String())
	tx := make([]byte, 1024*1024)
	_, err := io.ReadFull(rand.Reader, tx)
	if err != nil {
		b.Fatal(err)
	}

	rx := make([]byte, 65536)

	conn, err := net.DialTCP("tcp", nil, addr)
	if err != nil {
		b.Fatal(err)
		return
	}

	b.Log("sending", len(tx), "bytes for", b.N, "times")
	b.ReportAllocs()
	b.SetBytes(int64(len(tx)))
	b.ResetTimer()
	go func() {
		for i := 0; i < numLoops; i++ {
			_, err := conn.Write(tx)
			if err != nil {
				b.Fatal(err)
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
	conn.Close()
}
