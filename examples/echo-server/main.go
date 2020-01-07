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
					w.CloseConn(res.Fd)
					continue
				}

				// handle connection close
				if res.Size == 0 {
					log.Println("client closed")
					w.CloseConn(res.Fd)
					continue
				}

				// send back everything, we won't start to read again until write completes.
				buf := make([]byte, res.Size)
				copy(buf, res.Buffer[:res.Size])
				w.Write(nil, res.Fd, buf)

			case gaio.OpWrite: // write completion event
				// handle unexpected write error
				if res.Err != nil {
					log.Println("write error")
					w.CloseConn(res.Fd)
					continue
				}
				// since write has completed, let's start read on this 'fd' again
				w.Read(nil, res.Fd, nil)
			}
		}
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Println(err)
			return
		}

		// this conn will be monitored by Watcher
		fd, err := w.NewConn(conn)
		if err != nil {
			log.Println(err)
			return
		}
		log.Println("new client", conn.RemoteAddr())

		// submit the first async read IO request
		err = w.Read(nil, fd, nil)
		if err != nil {
			log.Println(err)
			return
		}
	}
}
