package main

import (
	"log"
	"net"

	"github.com/xtaci/gaio"
)

// this goroutine will wait for all io events, and sents back everything it received
// in async way
func echoServer(w *gaio.Watcher) {
	for {
		// loop wait for any IO events
		results, err := w.WaitIO()
		if err != nil {
			log.Println(err)
			return
		}

		for _, res := range results {
			switch res.Operation {
			case gaio.OpRead: // read completion event
				if res.Error == nil {
					// send back everything, we won't start to read again until write completes.
					// submit an async write request
					w.Write(nil, res.Conn, res.Buffer[:res.Size])
				}
			case gaio.OpWrite: // write completion event
				if res.Error == nil {
					// since write has completed, let's start read on this conn again
					w.Read(nil, res.Conn, res.Buffer[:cap(res.Buffer)])
				}
			}
		}
	}
}

func main() {
	w, err := gaio.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close()

	go echoServer(w)

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		log.Fatal(err)
	}
	log.Println("echo server listening on", ln.Addr())

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Println(err)
			return
		}
		log.Println("new client", conn.RemoteAddr())

		// submit the first async read IO request
		err = w.Read(nil, conn, make([]byte, 128))
		if err != nil {
			log.Println(err)
			return
		}
	}
}
