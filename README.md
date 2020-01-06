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

## Benchmark

**Throughput test with 64KB buffer**

```
=== Macbook Pro ===
BenchmarkEcho-4   	2019/12/24 15:42:16
    1485	    743233 ns/op	1410.83 MB/s	    1593 B/op	      32 allocs/op
--- BENCH: BenchmarkEcho-4
    aio_test.go:180: sending 1048576 bytes for 1 times
    aio_test.go:180: sending 1048576 bytes for 100 times
    aio_test.go:180: sending 1048576 bytes for 1485 times

=== Raspberry Pi 4===
BenchmarkEcho-4         2020/01/05 22:15:29
     500           3465074 ns/op         302.61 MB/s        4104 B/op        160 allocs/op
--- BENCH: BenchmarkEcho-4
    aio_test.go:288: sending 1048576 bytes for 1 times
    aio_test.go:288: sending 1048576 bytes for 100 times
    aio_test.go:288: sending 1048576 bytes for 500 times
```

**Concurrent echo test, each connection send and receive 1KB data. 8K connections**

```
=== Macbook Pro ===
$ go test -v -run 8k
=== RUN   Test8k
--- PASS: Test8k (1.09s)
    aio_test.go:325: completed: 8388608
    aio_test.go:41: watcher closed
PASS
ok  	github.com/xtaci/gaio	1.807s

=== Raspberry Pi 4===
➜  gaio git:(master) go test -v -run 8k
=== RUN   Test8k
--- PASS: Test8k (2.09s)
    aio_test.go:325: completed: 8388608
    aio_test.go:41: watcher closed
PASS
ok      github.com/xtaci/gaio   2.481s

```


## Usage

More to be found: https://github.com/xtaci/gaio/tree/master/examples

## License

`gaio` source code is available under the MIT [License](/LICENSE).

## Status

Alpha
