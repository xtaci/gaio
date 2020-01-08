package main

import (
	"log"
	"net"

	"github.com/xtaci/gaio"
)

func main() {
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		log.Fatal(err)
	}

	log.Println("echo server listening on", ln.Addr())

	w, err := gaio.CreateWatcher(4096)
	if err != nil {
		log.Fatal(err)
	}

	// this goroutine will wait for all io events, and sents back everything it received
	go func() {
		for {
			// wait for any IO events
			res, err := w.WaitIO()
			if err != nil {
				log.Println(err)
				return
			}

			switch res.Op {
			case gaio.OpRead: // read completion event
				// handle unexpected read error
				if res.Err != nil {
					log.Println("read error")
					continue
				}

				// handle connection close
				if res.Size == 0 {
					log.Println("client closed")
					continue
				}

				// send back everything, we won't start to read again until write completes.
				buf := make([]byte, res.Size)
				copy(buf, res.Buffer[:res.Size])
				w.Write(nil, res.Conn, buf)

			case gaio.OpWrite: // write completion event
				// handle unexpected write error
				if res.Err != nil {
					log.Println("write error")
					continue
				}
				// since write has completed, let's start read on this 'fd' again
				w.Read(nil, res.Conn, nil)
			}
		}
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Println(err)
			return
		}
		log.Println("new client", conn.RemoteAddr())

		// submit the first async read IO request
		err = w.Read(nil, conn, nil)
		if err != nil {
			log.Println(err)
			return
		}
	}
}
