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

	// this goroutine will wait for all io events, and sents back everything it received
	go func() {
		for {
			// wait for any IO events
			res, err := gaio.WaitIO()
			if err != nil {
				log.Println(err)
				return
			}

			switch res.Operation {
			case gaio.OpRead: // read completion event
				// handle unexpected read error
				if res.Error != nil {
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
				gaio.Write(nil, res.Conn, buf)

			case gaio.OpWrite: // write completion event
				// handle unexpected write error
				if res.Error != nil {
					log.Println("write error")
					continue
				}
				// since write has completed, let's start read on this 'fd' again
				gaio.Read(nil, res.Conn, nil)
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
		err = gaio.Read(nil, conn, nil)
		if err != nil {
			log.Println(err)
			return
		}
	}
}
