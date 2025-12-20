# gaio

[![GoDoc][1]][2] [![MIT licensed][3]][4] [![Build Status][5]][6] [![Go Report Card][7]][8] [![Coverage Statusd][9]][10]

[1]: https://godoc.org/github.com/xtaci/gaio?status.svg
[2]: https://pkg.go.dev/github.com/xtaci/gaio
[3]: https://img.shields.io/badge/license-MIT-blue.svg
[4]: LICENSE
[5]: https://img.shields.io/github/created-at/xtaci/gaio
[6]: https://img.shields.io/github/created-at/xtaci/gaio
[7]: https://goreportcard.com/badge/github.com/xtaci/gaio
[8]: https://goreportcard.com/report/github.com/xtaci/gaio
[9]: https://codecov.io/gh/xtaci/gaio/branch/master/graph/badge.svg
[10]: https://codecov.io/gh/xtaci/gaio

<img src="assets/logo.png" alt="gaio" height="300px" /> 

## 简介
[English](README.md)

在典型的 Go 网络程序中，通常使用 `conn := lis.Accept()` 接受连接，然后使用 `go func(net.Conn)` 启动一个 goroutine 来处理传入数据。接着，使用 `buf := make([]byte, 4096)` 分配缓冲区，并使用 `conn.Read(buf)` 等待数据。

对于管理 10,000+ 连接且频繁处理短消息（例如 <512 字节）的服务器，上下文切换的成本显著超过了消息接收的成本——每次上下文切换需要超过 1,000 个 CPU 周期，在 2.1 GHz 处理器上大约需要 600 纳秒。

通过边缘触发（Edge-Triggered）I/O 多路复用消除“每个连接一个 goroutine”的模型，可以节省通常为每个 goroutine 分配的 2KB (R) + 2KB (W) 栈空间。此外，使用内部交换缓冲区消除了 `buf := make([]byte, 4096)` 的需求，以牺牲少量性能为代价换取内存效率。

gaio 库实现了 Proactor 模式，有效地在内存限制和性能要求之间取得了平衡。

## 工作原理

`dup` 函数用于从 `net.Conn` 复制文件描述符：

```
NAME
       dup, dup2, dup3 - duplicate a file descriptor

LIBRARY
       Standard C library (libc, -lc)

SYNOPSIS
       #include <unistd.h>

       int dup(int oldfd);
       int dup2(int oldfd, int newfd);

       #define _GNU_SOURCE             /* See feature_test_macros(7) */
       #include <fcntl.h>              /* Definition of O_* constants */
       #include <unistd.h>

       int dup3(int oldfd, int newfd, int flags);

DESCRIPTION
       The dup() system call allocates a new file descriptor that refers to the same open file description as the de‐
       scriptor oldfd.  (For an explanation of open file descriptions, see open(2).)  The new file descriptor  number
       is guaranteed to be the lowest-numbered file descriptor that was unused in the calling process.

       After  a  successful return, the old and new file descriptors may be used interchangeably.  Since the two file
       descriptors refer to the same open file description, they share file offset and file status flags;  for  exam‐
       ple,  if  the  file  offset  is  modified by using lseek(2) on one of the file descriptors, the offset is also
       changed for the other file descriptor.

       The two file descriptors do not share file descriptor flags (the close-on-exec flag).  The close-on-exec  flag
       (FD_CLOEXEC; see fcntl(2)) for the duplicate descriptor is off.
```

## 特性

- **高性能：** 在高频交易环境中经过实战检验，在单台 HVM 服务器上实现了 30K–40K RPS。
- **可扩展性：** 专为 C10K+ 并发连接设计，优化了并行性和单连接吞吐量。
- **灵活的缓冲：** 使用带有 nil 缓冲区的 `Read(ctx, conn, buffer)` 来利用内部交换缓冲区。
- **非侵入式集成：** 兼容 `net.Listener` 和 `net.Conn`（支持 `syscall.RawConn`），能够无缝集成到现有应用程序中。
- **高效的上下文切换：** 最小化小消息的上下文切换开销，非常适合高频消息交换。
- **可定制的托管：** 应用程序可以控制何时将 `net.Conn` 托管给 gaio，例如在握手之后或特定的 `net.TCPConn` 配置之后。
- **背压处理：** 应用程序可以控制读/写请求的提交时机，从而实现每连接的背压管理，以便在必要时限制发送——特别是在将数据从较快的源 (A) 传输到较慢的目的地 (B) 时非常有用。
- **轻量级且易于维护：** 大约 1,000 行代码，便于调试和维护。
- **跨平台支持：** 兼容 Linux 和 BSD。

## 约定

- **连接托管：** 一旦向 `gaio.Watcher` 提交了 `net.Conn` 的异步读/写请求，该连接即被托管给 watcher。随后调用 `conn.Read` 或 `conn.Write` 将返回错误，但通过 `SetReadBuffer()`、`SetWriteBuffer()`、`SetLinger()`、`SetKeepAlive()` 和 `SetNoDelay()` 设置的 TCP 属性将被保留。
  
- **资源管理：** 当不再需要连接时，调用 `Watcher.Free(net.Conn)` 以立即关闭套接字并释放资源。如果忘记调用 `Watcher.Free()`，运行时垃圾回收器将在 `net.Conn` 不再被其他地方引用时清理系统资源。同样，如果未能调用 `Watcher.Close()`，垃圾回收器将在 watcher 不再被引用时清理所有相关资源。

- **负载均衡：** 对于连接负载均衡，创建多个 `gaio.Watcher` 实例并使用您喜欢的策略分发 `net.Conn`。对于接收器（acceptor）负载均衡，请使用 `go-reuseport` 作为监听器。

- **安全的读请求：** 当提交带有 nil 缓冲区的读请求时，从 `Watcher.WaitIO()` 返回的 `[]byte` 切片在下一次 `Watcher.WaitIO()` 调用之前保持有效。

## 快速开始 (TL;DR)

```go
package main

import (
        "log"
        "net"

        "github.com/xtaci/gaio"
)

// 这个 goroutine 等待所有 I/O 事件并以异步方式回发接收到的所有内容
func echoServer(w *gaio.Watcher) {
        for {
                // 循环等待任何 IO 事件
                results, err := w.WaitIO()
                if err != nil {
                        log.Println(err)
                        return
                }

                for _, res := range results {
                        switch res.Operation {
                        case gaio.OpRead: // 读完成事件
                                if res.Error == nil {
                                        // 回发所有内容，在写完成之前我们不会再次开始读取。
                                        // 提交一个异步写请求
                                        w.Write(nil, res.Conn, res.Buffer[:res.Size])
                                }
                        case gaio.OpWrite: // 写完成事件
                                if res.Error == nil {
                                        // 既然写已完成，让我们再次开始读取此连接
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

                // 提交第一个异步读 IO 请求
                err = w.Read(nil, conn, make([]byte, 128))
                if err != nil {
                        log.Println(err)
                        return
                }
        }
}

```

### 更多示例

<details>
	<summary> 推送服务器 (Push server) </summary>
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
        // 只需将 net.Listen 替换为 reuseport.Listen，其他一切与 push-server 相同
        // ln, err := reuseport.Listen("tcp", "localhost:0")
        ln, err := net.Listen("tcp", "localhost:0")
        if err != nil {
                log.Fatal(err)
        }

        log.Println("pushing server listening on", ln.Addr(), ", use telnet to receive push")

        // 创建一个 watcher
        w, err := gaio.NewWatcher()
        if err != nil {
                log.Fatal(err)
        }

        // 通道
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

        // 主逻辑循环，类似于您的程序核心循环。
        go func() {
                var conns []net.Conn
                for {
                        select {
                        case res := <-chIO: // 从 watcher 接收 IO 事件
                                if res.Error != nil {
                                        continue
                                }
                                conns = append(conns, res.Conn)
                        case t := <-ticker.C: // 接收 ticker 事件
                                push := []byte(fmt.Sprintf("%s\n", t))
                                // 所有连接将接收相同的 'push' 内容
                                for _, conn := range conns {
                                        w.Write(nil, conn, push)
                                }
                                conns = nil
                        case conn := <-chConn: // 接收新连接事件
                                conns = append(conns, conn)
                        }
                }
        }()

        // 此循环持续接受连接并发送到主循环
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

## 文档

有关完整文档，请参阅关联的 [Godoc](https://godoc.org/github.com/xtaci/gaio)。

## 基准测试

| 测试用例 | 64KB 缓冲区吞吐量测试 |
|:-------------:|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| 描述 | 客户端持续向服务器发送 64KB 数据。服务器读取数据并将其回显。客户端持续接收直到所有字节都成功接收。 |
| 命令 | `go test -v -run=^$ -bench Echo` |
| Macbook Pro | 1695.27 MB/s 518 B/op 4 allocs/op|
| Linux AMD64 | 1883.23 MB/s 518 B/op 4 allocs/op|
| Raspberry Pi4 | 354.59 MB/s 334 B/op 4 allocs/op|

| 测试用例 | 8K 并发连接回显测试 |
|:-------------:|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
|描述| 启动 8192 个客户端，每个向服务器发送 1KB 数据。服务器读取并回显数据，每个客户端持续接收直到所有字节都成功接收。|
| 命令 | `go test -v -run=8k` |
| Macbook Pro | 1.09s |
| Linux AMD64 | 0.94s |
| Raspberry Pi4 | 2.09s |

## 测试指令
在 macOS 上，您需要增加最大打开文件限制以运行基准测试。

```bash
sysctl -w kern.ipc.somaxconn=4096
sysctl -w kern.maxfiles=100000
sysctl -w kern.maxfilesperproc=100000
sysctl -w net.inet.ip.portrange.first=1024
sysctl -w net.inet.ip.portrange.last=65535

ulimit -S -n 65536
```

### 回归测试

![regression](assets/regression.png)

X -> 并发连接数, Y -> 完成时间（秒）

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

## 常见问题 (FAQ)
1. 如果您遇到如下错误：

```
# github.com/xtaci/gaio [github.com/xtaci/gaio.test]
./aio_linux.go:155:7: undefined: setAffinity
./watcher.go:588:4: undefined: setAffinity
FAIL	github.com/xtaci/gaio [build failed]
FAIL
```

请确保已安装 gcc/clang。

## 许可证

`gaio` 源代码在 MIT [许可证](/LICENSE)下可用。

## 参考资料

* https://zhuanlan.zhihu.com/p/102890337 -- gaio小记
* https://github.com/xtaci/grasshopper -- 由 gaio 构成的安全链式中继器
* https://github.com/golang/go/issues/15735 -- net: add mechanism to wait for readability on a TCPConn
* https://en.wikipedia.org/wiki/C10k_problem -- C10K
* https://golang.org/src/runtime/netpoll_epoll.go -- epoll in golang 
* https://www.freebsd.org/cgi/man.cgi?query=kqueue&sektion=2 -- kqueue
* https://idea.popcount.org/2017-02-20-epoll-is-fundamentally-broken-12/ -- epoll is fundamentally broken
* https://en.wikipedia.org/wiki/Transmission_Control_Protocol#Flow_control -- TCP Flow Control 
* http://www.idc-online.com/technical_references/pdfs/data_communications/Congestion_Control.pdf -- Back-pressure

## 状态

稳定
