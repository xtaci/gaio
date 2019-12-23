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

For a typical golang network program, you would `lis.Accept()` a connection and `go func(net.Conn)` , then `buf:=make([]bytes)` and finally waits on `conn.Read(buf)`, this is wasteful in memory, especially for a server holding >10K connections and most of them are in idle. 

Then you need at least **2KB(goroutine stack) + 4KB(buffer)** for ONE connection, at least **6KB x 10K = 60MB** in total, this number will be doubled with `conn.Write()`.

This library is designed to fix.

## Guarantees

1. Only a fixed number of goroutines will be created per **Watcher**(the core object of this library).
2. The IO-completion notification on a **Watcher** is sequential, that means buffer can be reused in some pattern.
3. You can only have one reader and one writer on one specific connection at one time, newer ones will replace the old ones.
4. Non-intrusive design, this library works with `net.Listener` and `net.Conn`. (with `syscall.RawConn` support)
5. Support for Linux, BSD.

## Documentation

For complete documentation, see the associated [Godoc](https://godoc.org/github.com/xtaci/gaio).

## Benchmark

## Usage

## Status

**Status: Work in progress**
