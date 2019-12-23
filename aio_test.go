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
)

func init() {

	go http.ListenAndServe(":6060", nil)
}

func echoServer(t testing.TB) net.Listener {
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}

	w, err := CreateWatcher()
	if err != nil {
		t.Fatal(err)
	}

	chRx := make(chan OpResult)
	chTx := make(chan OpResult)
	// ping-pong scheme echo server
	go func() {
		for {
			select {
			case res := <-chRx:
				if res.Err != nil {
					log.Println("read error:", res.Err, res.Size)
					w.StopWatch(res.Fd)
					continue
				}

				if res.Size == 0 {
					log.Println("client closed")
					w.StopWatch(res.Fd)
					continue
				}

				// write the data, we won't start to read again until write completes.
				w.Write(res.Fd, res.Buffer[:res.Size:cap(res.Buffer)], chTx)
			case res := <-chTx:
				if res.Err != nil {
					log.Println("write error:", res.Err, res.Size)
					w.StopWatch(res.Fd)
				}
				// write complete, start read again
				w.Read(res.Fd, res.Buffer[:cap(res.Buffer)], chRx)
			}
		}
	}()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				log.Println(err)
				return
			}

			fd, err := w.Watch(conn)
			if err != nil {
				log.Println(err)
				return
			}

			log.Println("watching", conn.RemoteAddr(), "fd:", fd)

			// kick off
			err = w.Read(fd, make([]byte, 1024), chRx)
			if err != nil {
				log.Println(err)
				return
			}
		}
	}()
	return ln
}

func TestEchoTiny(t *testing.T) {
	ln := echoServer(t)
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

func TestEchoHuge(t *testing.T) {
	ln := echoServer(t)
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

func BenchmarkEcho(b *testing.B) {
	ln := echoServer(b)

	addr, _ := net.ResolveTCPAddr("tcp", ln.Addr().String())
	tx := []byte("hello world")
	rx := make([]byte, len(tx))

	conn, err := net.DialTCP("tcp", nil, addr)
	if err != nil {
		b.Fatal(err)
		return
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := conn.Write(tx)
		if err != nil {
			b.Fatal(err)
		}
		_, err = conn.Read(rx)
		if err != nil {
			b.Fatal(err)
		}
		//		log.Println(i, b.N)
	}
	conn.Close()
}
