// Copyright (c) 2019 xtaci
//
// Permission is hereby granted, free of charge, to any person obtaining a copy of
// this software and associated documentation files (the "Software"), to deal in
// the Software without restriction, including without limitation the rights to
// use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
// the Software, and to permit persons to whom the Software is furnished to do so,
// subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS
// FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR
// COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER
// IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN
// CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

package main

import (
	"fmt"
	"log"
	"net"
	"time"

	"github.com/xtaci/gaio"
)

func main() {
	// Replace net.Listen with reuseport.Listen to use a reuse-port listener.
	// ln, err := reuseport.Listen("tcp", "localhost:0")
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		log.Fatal(err)
	}

	log.Println("pushing server listening on", ln.Addr(), ", use telnet to receive push")

	// Create a watcher to manage asynchronous writes.
	w, err := gaio.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}

	// Forward accepted connections and completed I/O to the main loop.
	ticker := time.NewTicker(time.Second)
	chConn := make(chan net.Conn)
	chIO := make(chan gaio.OpResult)

	// Forward watcher completions to the main loop.
	go func() {
		for {
			results, err := w.WaitIO()
			if err != nil {
				log.Println(err)
				return
			}

			for _, res := range results {
				chIO <- res
			}
		}
	}()

	// Keep connection management and periodic broadcasts in one loop.
	go func() {
		var conns []net.Conn
		for {
			select {
			case res := <-chIO: // receive I/O events from watcher
				if res.Error != nil {
					continue
				}
				conns = append(conns, res.Conn)
			case t := <-ticker.C: // receive ticker events
				push := fmt.Appendf(nil, "%s\n", t)
				// Send the same payload to each connection collected this tick.
				for _, conn := range conns {
					w.Write(nil, conn, push)
				}
				conns = nil
			case conn := <-chConn: // receive new connection events
				conns = append(conns, conn)
			}
		}
	}()

	// Accept connections and pass them to the main loop.
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Println(err)
			return
		}
		chConn <- conn
	}
}
