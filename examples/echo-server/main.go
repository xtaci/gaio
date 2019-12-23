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

	rx := make([]byte, 1024)
	chRx := make(chan gaio.OpResult)
	go func() {
		chTx := make(chan gaio.OpResult)
		for {
			select {
			case res := <-chRx:
				if res.Size > 0 {
					tx := make([]byte, res.Size)
					copy(tx, rx)
					w.Write(res.Fd, tx, chTx)
				} else if res.Size == 0 && res.Err == nil {
					log.Println("client closed")
				}

			case res := <-chTx:
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

		// kick off
		err = w.Read(fd, rx, chRx)
		if err != nil {
			log.Println(err)
			return
		}
	}
}
