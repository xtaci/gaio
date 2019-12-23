package gaio

import (
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
	go func() {
		//var n int32
		//var m int32
		// ping-pong scheme echo server
		chTx := make(chan OpResult)

		// per connetion read buffers
		readBuffers := make(map[int][]byte)

		for {
			select {
			case res := <-chRx:
				readBuffers[res.Fd] = res.Buffer
				if res.Err != nil {
					log.Println("read error")
					w.StopWatch(res.Fd)
					continue
				}

				if res.Size == 0 {
					log.Println("client closed")
					w.StopWatch(res.Fd)
					continue
				}

				// per connection write buffer
				tx := make([]byte, res.Size)
				//log.Println("read:", atomic.AddInt32(&n, int32(res.Size)))
				copy(tx, res.Buffer)
				w.Write(res.Fd, tx, chTx)
			case res := <-chTx:
				if res.Err != nil {
					log.Println("write error:", res.Err)
					w.StopWatch(res.Fd)
				}
				/*
					if res.Size > 0 {
						log.Println("write:", atomic.AddInt32(&m, int32(res.Size)))
					}
				*/
				// start read again
				w.Read(res.Fd, readBuffers[res.Fd], chRx)
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
			rxBuf := make([]byte, 1024)
			err = w.Read(fd, rxBuf, chRx)
			if err != nil {
				log.Println(err)
				return
			}
		}
	}()
	return ln
}

func TestEcho(t *testing.T) {
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
	tx := make([]byte, 1024*1024)
	rx := make([]byte, len(tx))

	go func() {
		n, err := conn.Write(tx)
		if err != nil {
			t.Fatal(err)
		}
		t.Log("ping size", n)
	}()

	n, err := io.ReadFull(conn, rx)
	if err != nil {
		t.Fatal(err, n)
	}
	t.Log("pong size:", n)
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
		conn.Write(tx)
		conn.Read(rx)
		//		log.Println(i, b.N)
	}
	conn.Close()
}
