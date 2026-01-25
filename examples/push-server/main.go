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
	// By simply replacing net.Listen with reuseport.Listen, the rest stays the same.
	// ln, err := reuseport.Listen("tcp", "localhost:0")
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		log.Fatal(err)
	}

	log.Println("pushing server listening on", ln.Addr(), ", use telnet to receive push")

	// create a watcher
	w, err := gaio.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}

	// Channels
	ticker := time.NewTicker(time.Second)
	chConn := make(chan net.Conn)
	chIO := make(chan gaio.OpResult)

	// watcher.WaitIO goroutine
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

	// Main logic loop, like your program's core loop.
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
				push := []byte(fmt.Sprintf("%s\n", t))
				// all conns will receive the same 'push' content
				for _, conn := range conns {
					w.Write(nil, conn, push)
				}
				conns = nil
			case conn := <-chConn: // receive new connection events
				conns = append(conns, conn)
			}
		}
	}()

	// This loop keeps accepting connections and sends them to the main loop.
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Println(err)
			return
		}
		chConn <- conn
	}
}
