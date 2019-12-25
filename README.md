# gaio

Async-IO Library for golang

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

**Status: Work in progress**

## Introduction

For a typical golang network program, you would first `conn := lis.Accept()` to get a connection and `go func(net.Conn)` to start a goroutine for handling the incoming data, then you would `buf:=make([]byte, 4096)` to allocate some buffer and finally waits on `conn.Read(buf)`. This is wasteful in memory, especially for a server holding >10K connections and most of them are in idle. 

In this case, you need at least **2KB(goroutine stack) + 4KB(buffer)** for receiving data on one connection, at least **6KB x 10K = 60MB** in total, this number will be doubled with data sending goroutine, not counting the **context switches**

By eliminating **one goroutine per one connection scheme**, the 2KB goroutine stack can be saved, then by restricting delivery order or events, one **common buffer** can be shared for all incoming connections in a `Watcher`.

## Guarantees

1. Only a fixed number of goroutines will be created per **Watcher**(the core object of this library).
2. The IO-completion notification on a **Watcher** is sequential, that means buffer can be reused in some pattern.
3. Non-intrusive design, this library works with `net.Listener` and `net.Conn`. (with `syscall.RawConn` support)
4. Support for Linux, BSD.

## Documentation

For complete documentation, see the associated [Godoc](https://godoc.org/github.com/xtaci/gaio).

## Benchmark

An Echo-server with 64KB receiving buffer
```
BenchmarkEcho-4   	2019/12/24 15:42:16
    1485	    743233 ns/op	1410.83 MB/s	    1593 B/op	      32 allocs/op
--- BENCH: BenchmarkEcho-4
    aio_test.go:180: sending 1048576 bytes for 1 times
    aio_test.go:180: sending 1048576 bytes for 100 times
    aio_test.go:180: sending 1048576 bytes for 1485 times
```

## Usage

A simple echo server

```go
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
					w.StopWatch(res.Fd)
					continue
				}

				// handle connection close
				if res.Size == 0 {
					w.StopWatch(res.Fd)
					continue
				}

				// write the generated data, we won't start to read again until write completes.
				buf := make([]byte, res.Size)
				copy(buf, res.Buffer[:res.Size])
				w.Write(res.Fd, buf, chTx)
			case res := <-chTx:
				// handle unexpected write error
				if res.Err != nil {
					w.StopWatch(res.Fd)
					continue
				}
				// write complete, start read again
				w.Read(res.Fd, nil, chRx)
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

		// kick off the first read action on this conn
		err = w.Read(fd, nil, chRx)
		if err != nil {
			log.Println(err)
			return
		}
	}
}
```

More to be found: https://github.com/xtaci/gaio/tree/master/examples

## Status

**Status: Work in progress**
