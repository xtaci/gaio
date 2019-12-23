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

	w, err := gaio.CreateWatcher()
	if err != nil {
		log.Fatal(err)
	}

	// because read event happens in sequence,
	// only 1 global read buffer is required for a watcher.
	rx := make([]byte, 1024)
	chRx := make(chan gaio.OpResult)
	go func() {
		chTx := make(chan gaio.OpResult)
		for {
			select {
			case res := <-chRx:
				if res.Size > 0 {
					// make a write buffer and echo
					tx := make([]byte, res.Size)
					copy(tx, rx)
					// echo the data, we won't start read again
					// until write completes.
					w.Write(res.Fd, tx, chTx)
				} else if res.Size == 0 && res.Err == nil {
					log.Println("client closed")
				}

			case res := <-chTx:
				// write complete, start read again
				w.Read(res.Fd, rx, chRx)
			}
		}
	}()

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

		log.Println("new client", conn.RemoteAddr())

		// kick off the first read action on this conn
		err = w.Read(fd, rx, chRx)
		if err != nil {
			log.Println(err)
			return
		}
	}
}
