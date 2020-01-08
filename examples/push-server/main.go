package main

import (
	"fmt"
	"log"
	"net"
	"time"

	"github.com/xtaci/gaio"
)

func main() {
	// by simply replace net.Listen with reuseport.Listen, everything is the same as in push-server
	//ln, err := reuseport.Listen("tcp", "localhost:0")
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		log.Fatal(err)
	}

	log.Println("pushing server listening on", ln.Addr(), ", use telnet to receive push")

	// create a watcher with 4kb internal buffer
	w, err := gaio.NewWatcher(4096)
	if err != nil {
		log.Fatal(err)
	}

	// channel
	ticker := time.NewTicker(time.Second)
	chConn := make(chan net.Conn)
	chIO := make(chan gaio.OpResult)

	// watcher.WaitIO goroutine
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

	// main logic loop, like your program core loop.
	go func() {
		conns := make(map[net.Conn]bool)
		for {
			select {
			case res := <-chIO: // receive IO events from watcher
				if res.Err != nil {
					delete(conns, res.Conn)
				}
			case t := <-ticker.C: // receive ticker events
				push := []byte(fmt.Sprintf("%s\n", t))
				for fd := range conns {
					w.Write(nil, fd, push)
				}
			case conn := <-chConn: // receive new connection events
				conns[conn] = true
			}
		}
	}()

	// this loop keeps on accepting connections and send to main loop
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Println(err)
			return
		}
		log.Println("new client", conn.RemoteAddr())
		chConn <- conn
	}
}
