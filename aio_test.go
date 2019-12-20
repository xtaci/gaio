package ev

import (
	"log"
	"net"
	"sync"
	"testing"
)

func TestPush(t *testing.T) {
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}

	w := NewWatcher(4, 4096)
	data := make([]byte, 128)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			completed := func(c net.Conn, numWritten int, err error) {
				t.Log("push to", c, "with", numWritten, "bytes", "error:", err)
			}

			go func(conn net.Conn) {
				w.Watch(conn, Events{})
				w.Write(conn, WriteRequest{Out: data, WriteCompleted: completed})
			}(conn)
		}
	}()

	// discard client
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	tmp := make([]byte, 128)
	_, err = conn.Read(tmp)
	if err != nil {
		t.Fatal(err)
	}
}

func BenchmarkPush(b *testing.B) {
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		b.Fatal(err)
	}

	w := NewWatcher(4, 4096)
	data := make([]byte, 128)
	var x int32

	completed := func(c net.Conn, numWritten int, err error) {
		_ = x
		//log.Println(atomic.AddInt32(&x, 1))
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			go func(conn net.Conn) {
				w.Watch(conn, Events{})
				w.Write(conn, WriteRequest{Out: data, WriteCompleted: completed})
			}(conn)
		}
	}()

	tmp := make([]byte, 128)
	var wg sync.WaitGroup
	wg.Add(1024)
	for i := 0; i < 1024; i++ {
		go func() {
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				log.Println(err)
				b.Fatal(err)
			}
			_, err = conn.Read(tmp)
			if err != nil {
				b.Fatal(err)
			}
			conn.Close()
			wg.Done()
		}()
	}
	wg.Wait()
}
