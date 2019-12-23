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


Status: Work in progress

## Features

0. If memory is your first concern, this library may work for you.
1. Only a fixed number of goroutines will be created per Watcher.
2. The event notification on read-complete or write-complete is sequential, that means you can share a buffer to read or write among connections.
3. You can only have one reader and one writer on a specific connection at one time, newer ones will replace the old one.
4. Non-intrusive design, this library works on net.Listener and net.Conn. (with Syscall.RawConn support)
5. Support for Linux, BSD

