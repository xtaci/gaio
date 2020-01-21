# gaio

[![GoDoc][1]][2] [![MIT licensed][3]][4] [![Build Status][5]][6] [![Go Report Card][7]][8] [![Coverage Statusd][9]][10]

[1]: https://godoc.org/github.com/xtaci/gaio?status.svg
[2]: https://godoc.org/github.com/xtaci/gaio
[3]: https://img.shields.io/badge/license-MIT-blue.svg
[4]: LICENSE
[5]: https://travis-ci.org/xtaci/gaio.svg?branch=master
[6]: https://travis-ci.org/xtaci/gaio
[7]: https://goreportcard.com/badge/github.com/xtaci/gaio
[8]: https://goreportcard.com/report/github.com/xtaci/gaio
[9]: https://codecov.io/gh/xtaci/gaio/branch/master/graph/badge.svg
[10]: https://codecov.io/gh/xtaci/gaio

## Introduction

For a typical golang network program, you would first `conn := lis.Accept()` to get a connection and `go func(net.Conn)` to start a goroutine for handling the incoming data, then you would `buf:=make([]byte, 4096)` to allocate some buffer and finally waits on `conn.Read(buf)`. This is wasteful in memory, especially for a server holding >10K connections and most of them are in idle. 

In this case, you need at least **2KB(goroutine stack) + 4KB(buffer)** for receiving data on one connection, at least **6KB x 10K = 60MB** in total, this number will be doubled with data sending goroutine, not counting the **context switches**

```gaio``` is an [proactor pattern](https://en.wikipedia.org/wiki/Proactor_pattern) networking library satisfy both **memory requirements** and **performance requirements**.

By eliminating **one goroutine per one connection scheme** with **Edge-Triggered IO Multiplexing**, the 2KB goroutine stack can be saved, and by restricting delivery order or events, internal **mutual buffer** can be shared for all incoming connections in a `Watcher`.

## Guarantees

1. Only a fixed number of goroutines will be created per **Watcher**(the core object of this library).
2. The IO-completion notification on a **Watcher** is sequential, that means buffer can be reused in some pattern.
3. Non-intrusive design, this library works with `net.Listener` and `net.Conn`. (with `syscall.RawConn` support)
4. Support for Linux, BSD.

## Conventions

1. Once you submit an async read/write requests with related `net.Conn` to `gaio.Watcher`, this conn will be delegated to `gaio.Watcher` at first submit. Future use of this conn like `conn.Read` or `conn.Write` **will return error**.
2. If you decide not to use this connection anymore, you could call `gaio.Free(net.Conn)` to close socket and free related resources immediately.
3. If you forgot to call `gaio.Free(net.Conn)`, golang runtime garbage collection will do that for you. You don't have to worry about this.

## TL;DR

```go
package main

import (
        "log"
        "net"

        "github.com/xtaci/gaio"
)

// this goroutine will wait for all io events, and send back everything it received
// in async way
func echoServer() {
        for {
                // loop wait for any IO events
                res, err := gaio.WaitIO()
                if err != nil {
                        log.Println(err)
                        return
                }

                switch res.Operation {
                case gaio.OpRead: // read completion event
                        if res.Error == nil {
                                // send back everything, we won't start to read again until write completes.
                                buf := make([]byte, res.Size)
                                copy(buf, res.Buffer[:res.Size])
                                // submit an async write request
                                gaio.Write(nil, res.Conn, buf)
                        }
                case gaio.OpWrite: // write completion event
                        if res.Error == nil {
                                // since write has completed, let's start read on this conn again
                                gaio.Read(nil, res.Conn, nil)
                        }
                }
        }
}

func main() {
        go echoServer()
        
        ln, err := net.Listen("tcp", "localhost:0")
        if err != nil {
                log.Fatal(err)
        }

        for {
                conn, err := ln.Accept()
                if err != nil {
                        log.Println(err)
                        return
                }

                // submit the first async read IO request
                err = gaio.Read(nil, conn, nil)
                if err != nil {
                        log.Println(err)
                        return
                }
        }
}

```

### More examples

<details>
	<summary> Push server </summary>
        package main

```go
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
	// ln, err := reuseport.Listen("tcp", "localhost:0")
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
		var conns []net.Conn
		for {
			select {
			case res := <-chIO: // receive IO events from watcher
				if res.Error != nil {
					continue
				}
				conns = append(conns, res.Conn)
			case t := <-ticker.C: // receive ticker events
				push := []byte(fmt.Sprintf("%s\n", t))
				// all conn will receive the same 'push' content
				for _, conn := range conns {
					w.Write(nil, conn, push)
				}
				conns = nil
			case conn := <-chConn: // receive new connection events
				conns = append(conns, conn)
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
		chConn <- conn
	}
}

```
</details>

## Documentation

For complete documentation, see the associated [Godoc](https://godoc.org/github.com/xtaci/gaio).

## Benchmarks

| Test Case | Throughput test with 64KB buffer |
|:-------------:|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Description | A client keep on sending 64KB bytes to server, server keeps on reading and sending back whatever it received, the client keeps on receiving whatever the server sent back until all bytes received successfully |
| Command | `go test -v -run=^$ -bench Echo` |
| Macbook Pro | 1410.83 MB/s |
| Linux AMD64 | 1629.63 MB/s |
| Raspberry Pi4 | 317.13 MB/s |

| Test Case | 8K concurrent connection echo test |
|:-------------:|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
|Description| Start 8192 clients, each client send 1KB data to server, server keeps on reading and sending back whatever it received, the client keeps on receiving whatever the server sent back until all bytes received successfully.|
| Command | `go test -v -run=8k` |
| Macbook Pro | 1.09s |
| Linux AMD64 | 0.94s |
| Raspberry Pi4 | 2.09s |

### Regression

![regression](regression.png)

X -> number of concurrent connections, Y -> time of completion in seconds

```
Best-fit values	 
Slope	8.613e-005 ± 5.272e-006
Y-intercept	0.08278 ± 0.03998
X-intercept	-961.1
1/Slope	11610
 
95% Confidence Intervals	 
Slope	7.150e-005 to 0.0001008
Y-intercept	-0.02820 to 0.1938
X-intercept	-2642 to 287.1
 
Goodness of Fit	 
R square	0.9852
Sy.x	0.05421
 
Is slope significantly non-zero?	 
F	266.9
DFn,DFd	1,4
P Value	< 0.0001
Deviation from horizontal?	Significant
 
Data	 
Number of XY pairs	6
Equation	Y = 8.613e-005*X + 0.08278
```


## License

`gaio` source code is available under the MIT [License](/LICENSE).

## Articles

* https://zhuanlan.zhihu.com/p/102890337 -- gaio小记

## Status

Beta
