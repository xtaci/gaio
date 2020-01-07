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

## Documentation

For complete documentation, see the associated [Godoc](https://godoc.org/github.com/xtaci/gaio).

## Benchmarks

| Test Case | Throughput test with 64KB buffer |
|:-------------:|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Description | A client keep on sending 64KB bytes to server, server keeps on reading and sending back whatever it received, the client keeps on receiving whatever the server sent back until all bytes received successfully |
| Command | `go test -v -run=^$ -bench Echo` |
| Macbook Pro | 743233 ns/op	1410.83 MB/s	    1593 B/op	      32 allocs/op   |
| Linux AMD64 | 643445 ns/op	1629.63 MB/s	    3108 B/op	      32 allocs/op   |
| Raspberry Pi4 | 3306410 ns/op  317.13 MB/s    2180 B/op       32 allocs/op|

| Test Case | 8K concurrent connection echo test |
|:-------------:|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
|Description| Start 8192 clients, each client send 1KB data to server, server keeps on reading and sending back whatever it received, the client keeps on receiving whatever the server sent back until all bytes received successfully.|
| Command | `go test -v -run=8k` |
| Macbook Pro | 1.09s |
| Linux AMD64 | 0.94s |
| Raspberry Pi4 | 2.09s |



## Usage

More to be found: https://github.com/xtaci/gaio/tree/master/examples

## License

`gaio` source code is available under the MIT [License](/LICENSE).

## Status

Beta
