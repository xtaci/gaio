package main

import (
	"fmt"
	"log"
	"net"
	"time"

	"github.com/xtaci/gaio"
)

func main() {
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		log.Fatal(err)
	}

	log.Println("pushing server listening on", ln.Addr())

	w, err := gaio.CreateWatcher(4096)
	if err != nil {
		log.Fatal(err)
	}

	// chanel
	ticker := time.NewTicker(time.Second)
	chFd := make(chan int)
	chIO := make(chan gaio.OpResult)

	go func() {
		for {
			res, err := w.WaitIO()
			if err != nil {
				log.Println(err)
				return
			}
			chIO <- res
		}
	}()

	go func() {
		fds := make(map[int]bool)
		for {
			select {
			case res := <-chIO:
				if res.Err != nil {
					delete(fds, res.Fd)
				}
			case t := <-ticker.C:
				push := []byte(fmt.Sprintf("%s\n", t))
				for fd := range fds {
					w.Write(nil, fd, push)
				}
			case fd := <-chFd:
				fds[fd] = true
			}
		}
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Println(err)
			return
		}

		fd, err := w.NewConn(conn)
		if err != nil {
			log.Println(err)
			return
		}
		log.Println("new client", conn.RemoteAddr())
		chFd <- fd
	}
}
