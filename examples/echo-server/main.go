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
	"log"
	"net"

	"github.com/xtaci/gaio"
)

// echoServer handles completed I/O requests and echoes each read payload.
func echoServer(w *gaio.Watcher) {
	for {
		// Wait for one or more I/O completions.
		results, err := w.WaitIO()
		if err != nil {
			log.Println(err)
			return
		}

		for _, res := range results {
			switch res.Operation {
			case gaio.OpRead: // read completion event
				if res.Error == nil {
					// Submit the payload for writing before scheduling another read.
					w.Write(nil, res.Conn, res.Buffer[:res.Size])
				}
			case gaio.OpWrite: // write completion event
				if res.Error == nil {
					// Reuse the read buffer after the write has completed.
					w.Read(nil, res.Conn, res.Buffer[:cap(res.Buffer)])
				}
			}
		}
	}
}

func main() {
	w, err := gaio.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close()

	go echoServer(w)

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		log.Fatal(err)
	}
	log.Println("echo server listening on", ln.Addr())

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Println(err)
			return
		}
		log.Println("new client", conn.RemoteAddr())

		// Start asynchronous reads for the new connection.
		err = w.Read(nil, conn, make([]byte, 128))
		if err != nil {
			log.Println(err)
			return
		}
	}
}
