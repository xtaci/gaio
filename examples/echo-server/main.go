package main

import (
	"log"
	"net"

	"github.com/xtaci/gaio"
)

// this goroutine will wait for all io events, and sents back everything it received
// in async way
func echoServer() {
	for {
		// loop wait for any IO events
		res, err := gaio.WaitIO()
		if err != nil {
			log.Println(err)
			return
		}

		switch res.Operation {
		case gaio.OpRead: // read completion event
			if res.Error == nil {
				// send back everything, we won't start to read again until write completes.
				buf := make([]byte, res.Size)
				copy(buf, res.Buffer[:res.Size])
				// submit an async write request
				gaio.Write(nil, res.Conn, buf)
			}
		case gaio.OpWrite: // write completion event
			if res.Error == nil {
				// since write has completed, let's start read on this conn again
				gaio.Read(nil, res.Conn, nil)
			}
		}
	}
}

func main() {
	go echoServer()

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
		err = gaio.Read(nil, conn, nil)
		if err != nil {
			log.Println(err)
			return
		}
	}
}
