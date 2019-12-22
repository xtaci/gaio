# gaio

Async-IO Library for golang

[![GoDoc][1]][2]

[1]: https://godoc.org/github.com/xtaci/gaio?status.svg
[2]: https://godoc.org/github.com/xtaci/gaio

Status: Work in progress

## Features

1. Only a fixed number of goroutines will be created per Watcher.
2. The event notification on read-complete or write-complete is sequential, that means you can share a buffer to read or write among connections.
3. You can only have one reader and one writer on a specific connection at one time, newer ones will replace the old one.
4. Non-intrusive design, this library works on net.Listener and net.Conn. (with Syscall.RawConn support)
5. Support for Linux, BSD

