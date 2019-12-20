package ev

import (
	"log"
	"net"
	"sync"
	"testing"
	"time"
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

	sig := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			go func(conn net.Conn) {
				w.Watch(conn, Events{})
				<-sig
				w.Write(conn, WriteRequest{Out: data, WriteCompleted: completed})
			}(conn)
		}
	}()

	tmp := make([]byte, 128)
	var wg sync.WaitGroup
	wg.Add(b.N)
	log.Println("creating", b.N, "clients")
	for i := 0; i < b.N; i++ {
		go func() {
			defer wg.Done()
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				log.Println(err)
				return
			}
			defer conn.Close()
			_, err = conn.Read(tmp)
			if err != nil {
				log.Println(err)
				return
			}
		}()
		<-time.After(time.Millisecond)
	}
	b.ResetTimer()
	close(sig)
	wg.Wait()
}
