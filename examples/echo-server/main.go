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

	chRx := make(chan gaio.OpResult)
	chTx := make(chan gaio.OpResult)

	go func() {
		for {
			select {
			case res := <-chRx:
				// handle unexpected read error
				if res.Err != nil {
					log.Println("read error")
					w.StopWatch(res.Fd)
					continue
				}

				// handle connection close
				if res.Size == 0 {
					log.Println("client closed")
					w.StopWatch(res.Fd)
					continue
				}

				// write the data, we won't start to read again until write completes.
				buf := make([]byte, res.Size)
				copy(buf, res.Buffer[:res.Size])
				w.Write(res.Fd, buf, chTx, nil)
			case res := <-chTx:
				// handle unexpected write error
				if res.Err != nil {
					log.Println("write error")
					w.StopWatch(res.Fd)
					continue
				}
				// write complete, start read again
				w.Read(res.Fd, nil, chRx, nil)
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
		err = w.Read(fd, nil, chRx, nil)
		if err != nil {
			log.Println(err)
			return
		}
	}
}
