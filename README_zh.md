# gaio

[![GoDoc][1]][2] [![MIT licensed][3]][4] [![Build Status][5]][6] [![Go Report Card][7]][8] [![Coverage Statusd][9]][10]

[1]: https://godoc.org/github.com/xtaci/gaio\?status.svg
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

多数 Go 网络程序会 `conn := lis.Accept()` 拿到连接，随后 `go func(net.Conn)` 启动 goroutine，在内部分配 `buf := make([]byte, 4096)` 并 `conn.Read(buf)`。模式简单，却在 10,000+ 并发、报文多为 <512 字节时暴露瓶颈：上下文切换的代价（1,000+ CPU 周期，2.1 GHz 核心约 600 ns）远超真正收发数据的成本。

gaio 通过边缘触发（Edge-Triggered）I/O 多路复用与 Proactor 模式，彻底摆脱“每连接一个 goroutine”的开销：
- 大幅削减因 goroutine 栈分配带来的 2KB(R)+2KB(W) 常量成本；
- 复用内部交换缓冲区，避免为每次读取都 `make([]byte, 4096)`；
- 将事件循环与业务 goroutine 解耦，用有限资源支撑更高并发。

这些设计让 gaio 在有限内存预算与极致性能需求之间取得兼顾。

## 工作原理

`dup`/`dup2`/`dup3` 用来复制 `net.Conn` 的底层文件描述符，使 gaio 能独立调度 I/O，同时保持原 `net.Conn` 的生命周期和配置。

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
       The dup() system call allocates a new file descriptor that refers to the same open file description as the de-
       scriptor oldfd.  (For an explanation of open file descriptions, see open(2).)  The new file descriptor  number
       is guaranteed to be the lowest-numbered file descriptor that was unused in the calling process.

       After  a  successful return, the old and new file descriptors may be used interchangeably.  Since the two file
       descriptors refer to the same open file description, they share file offset and file status flags; for exam-
       ple,  if  the  file  offset  is  modified by using lseek(2) on one of the file descriptors, the offset is also
       changed for the other file descriptor.

       The two file descriptors do not share file descriptor flags (the close-on-exec flag).  The close-on-exec flag
       (FD_CLOEXEC; see fcntl(2)) for the duplicate descriptor is off.
```

## 特性

- **高性能：** 在高频交易等生产环境验证，单台 HVM 机器可稳定提供 30K–40K RPS。
- **可扩展：** 针对 C10K+ 并发优化单连接吞吐与整体调度，适应大规模短报文场景。
- **智能缓冲：** `Read(ctx, conn, nil)` 即可复用内部交换缓冲，避免重复分配。
- **原生兼容：** 与 `net.Listener`、`net.Conn`（包括 `syscall.RawConn`）无缝集成。
- **降低切换：** 极大减轻小包场景下的上下文切换损失。
- **托管自定义：** 可在握手、限流或任意阶段将连接委托给 watcher。
- **背压友好：** 业务可自行决定读/写请求提交时机，精准实现 per-conn 背压策略。
- **轻量易调：** 核心约 1,000 行代码，调试、审计和维护成本极低。
- **跨平台：** 同时支持 Linux 与 BSD。

## 约定

- **连接托管：** 一旦对某个 `net.Conn` 提交异步读/写请求，该连接即交由 `gaio.Watcher` 管理，后续直接调用 `conn.Read/Write` 会返回错误。但通过 `SetReadBuffer`、`SetWriteBuffer`、`SetLinger`、`SetKeepAlive`、`SetNoDelay` 所做的 TCP 设置会被保留。
- **资源释放：** 不再需要连接时调用 `Watcher.Free(net.Conn)` 可立即关闭套接字并回收资源；若忘记调用，GC 会在 `net.Conn` 无引用后清理。同样，如果未显式 `Watcher.Close()`，GC 也会在 watcher 无引用后处理善后。
- **负载均衡：** 可以创建多个 watcher 并使用任意策略分发连接；在监听层面建议配合 `go-reuseport` 做 acceptor 负载均衡。
- **读缓冲有效期：** 使用 nil 缓冲提交读请求时，`Watcher.WaitIO()` 返回的 `[]byte` 在下一次 `WaitIO` 调用前始终有效。

## 快速开始 (TL;DR)

```go
package main

import (
        "log"
        "net"

        "github.com/xtaci/gaio"
)

// echoServer 消费所有 I/O 事件，并把读到的数据异步写回
func echoServer(w *gaio.Watcher) {
        for {
                results, err := w.WaitIO()
                if err != nil {
                        log.Println(err)
                        return
                }

                for _, res := range results {
                        switch res.Operation {
                        case gaio.OpRead:
                                if res.Error == nil {
                                        w.Write(nil, res.Conn, res.Buffer[:res.Size])
                                }
                        case gaio.OpWrite:
                                if res.Error == nil {
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

                if err := w.Read(nil, conn, make([]byte, 128)); err != nil {
                        log.Println(err)
                        return
                }
        }
}
```

### 更多示例

<details>
	<summary>推送服务器 (Push server)</summary>

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
        // 替换为 reuseport.Listen 即可实现多实例监听
        // ln, err := reuseport.Listen("tcp", "localhost:0")
        ln, err := net.Listen("tcp", "localhost:0")
        if err != nil {
                log.Fatal(err)
        }

        log.Println("pushing server listening on", ln.Addr(), ", use telnet to receive push")

        w, err := gaio.NewWatcher()
        if err != nil {
                log.Fatal(err)
        }

        ticker := time.NewTicker(time.Second)
        chConn := make(chan net.Conn)
        chIO := make(chan gaio.OpResult)

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

        go func() {
                var conns []net.Conn
                for {
                        select {
                        case res := <-chIO:
                                if res.Error != nil {
                                        continue
                                }
                                conns = append(conns, res.Conn)
                        case t := <-ticker.C:
                                push := []byte(fmt.Sprintf("%s\n", t))
                                for _, conn := range conns {
                                        w.Write(nil, conn, push)
                                }
                                conns = nil
                        case conn := <-chConn:
                                conns = append(conns, conn)
                        }
                }
        }()

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

完整 API 说明请查阅 [Godoc](https://godoc.org/github.com/xtaci/gaio)。

## 基准测试

| 测试用例 | 64KB 缓冲区吞吐量测试 |
|:-------------:|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| 描述 | 客户端持续向服务器发送 64KB 数据，服务器读取后立即回显，客户端持续接收直到全部字节完成。|
| 命令 | `go test -v -run=^$ -bench Echo` |
| Macbook Pro | 1695.27 MB/s 518 B/op 4 allocs/op |
| Linux AMD64 | 1883.23 MB/s 518 B/op 4 allocs/op |
| Raspberry Pi4 | 354.59 MB/s 334 B/op 4 allocs/op |

| 测试用例 | 8K 并发连接回显测试 |
|:-------------:|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| 描述 | 启动 8192 个客户端，每个发送 1KB 数据；服务器回显，直至所有客户端完全收齐。|
| 命令 | `go test -v -run=8k` |
| Macbook Pro | 1.09s |
| Linux AMD64 | 0.94s |
| Raspberry Pi4 | 2.09s |

## 测试指令

在 macOS 上运行基准前，请先提升最大文件描述符限制：

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

X -> 并发连接数，Y -> 完成时间（秒）

```
Best-fit values 
Slope8.613e-005 ± 5.272e-006
Y-intercept0.08278 ± 0.03998
X-intercept-961.1
1/Slope11610
 
95% Confidence Intervals 
Slope7.150e-005 to 0.0001008
Y-intercept-0.02820 to 0.1938
X-intercept-2642 to 287.1
 
Goodness of Fit 
R square0.9852
Sy.x0.05421
 
Is slope significantly non-zero? 
F266.9
DFn,DFd1,4
P Value< 0.0001
Deviation from horizontal?Significant
 
Data 
Number of XY pairs6
EquationY = 8.613e-005*X + 0.08278
```

## 常见问题 (FAQ)

1. 编译时报错：

```
# github.com/xtaci/gaio [github.com/xtaci/gaio.test]
./aio_linux.go:155:7: undefined: setAffinity
./watcher.go:588:4: undefined: setAffinity
FAIL    github.com/xtaci/gaio [build failed]
FAIL
```

   请确认系统已安装 gcc 或 clang。

## 许可证

`gaio` 以 MIT [许可证](/LICENSE) 发布。

## 参考资料

* https://zhuanlan.zhihu.com/p/102890337 -- gaio 小记
* https://github.com/xtaci/grasshopper -- 基于 gaio 的安全链式中继器
* https://github.com/golang/go/issues/15735 -- net: add mechanism to wait for readability on a TCPConn
* https://en.wikipedia.org/wiki/C10k_problem -- C10K
* https://golang.org/src/runtime/netpoll_epoll.go -- epoll in golang
* https://www.freebsd.org/cgi/man.cgi\?query\=kqueue\&sektion\=2 -- kqueue
* https://idea.popcount.org/2017-02-20-epoll-is-fundamentally-broken-12/ -- epoll is fundamentally broken
* https://en.wikipedia.org/wiki/Transmission_Control_Protocol\#Flow_control -- TCP Flow Control 
* http://www.idc-online.com/technical_references/pdfs/data_communications/Congestion_Control.pdf -- Back-pressure

## 状态

稳定
