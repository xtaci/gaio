# Project Analysis for AI Agents

This document provides a high-level overview of the `gaio` project structure, architectural decisions, and key components to assist AI agents in understanding and modifying the codebase.

## Project Overview

`gaio` (Goroutine-based Async IO) is a high-performance networking library for Go that implements the **Proactor pattern** using **Edge-Triggered I/O Multiplexing**. It is designed to handle C10K+ concurrent connections with minimal memory footprint and context switching overhead compared to the standard `net` library's one-goroutine-per-connection model.

## Core Architecture

### 1. The Watcher (`watcher.go`)
The `Watcher` is the central component of the library. It manages:
- **Event Loop**: Monitors file descriptors for I/O events.
- **I/O Queues**: Maintains lists of pending read and write requests (`aiocb`).
- **Buffer Management**: Implements a triple-buffering mechanism (`swapBufferFront`, `swapBufferMiddle`, `swapBufferBack`) to optimize memory usage for read operations.
- **Lifecycle**: Handles connection registration, delegation, and cleanup.

### 2. The Poller (`aio_linux.go`, `aio_bsd.go`)
The `poller` struct abstracts the underlying OS-specific I/O multiplexing mechanism:
- **Linux**: Uses `epoll` with `EPOLLET` (Edge-Triggered).
- **BSD (macOS, FreeBSD)**: Uses `kqueue`.

### 3. Async I/O Control Block (`aiocb`)
Represents a single asynchronous I/O request. It contains:
- The operation type (Read/Write).
- The target file descriptor.
- The buffer to use.
- Context and user-defined data.

### 4. File Descriptor Descriptor (`fdDesc`)
Maintains the state for a specific file descriptor, including:
- `readers`: A linked list of pending read requests.
- `writers`: A linked list of pending write requests.
- `ptr`: A pointer to the associated `net.Conn` (stored as `uintptr` to avoid GC interference in certain maps).

## Key Design Decisions

- **Duping File Descriptors**: `gaio` duplicates the file descriptor from `net.Conn` using `dup()`. This allows `gaio` to manage the FD independently while the original `net.Conn` remains valid (though it should not be used for I/O once delegated).
- **Triple Buffering**: To avoid allocating a buffer for every connection, `gaio` uses a shared swap buffer system.
- **Goroutine Pool Avoidance**: Unlike traditional reactor patterns that might spawn a goroutine per event, `gaio` pushes completion events to a channel (`chResults`), allowing the user to control the concurrency model for processing results.
- **Garbage Collection Safety**: Special care is taken with `uintptr` and `KeepAlive` to ensure `net.Conn` objects are not garbage collected while they are being managed by `gaio`.

## File Structure Map

- **`watcher.go`**: Core logic for the `Watcher`, event loop, and request processing.
- **`aio_linux.go`**: Linux-specific `epoll` implementation.
- **`aio_bsd.go`**: BSD-specific `kqueue` implementation.
- **`aio_generic.go`**: Generic helper functions and definitions.
- **`affinity_*.go`**: CPU affinity settings (binding the poller thread to a specific CPU).
- **`time.go`**: Timer heap implementation for handling I/O timeouts.
- **`examples/`**:
    - `echo-server/`: Simple echo server demonstrating basic usage.
    - `push-server/`: Push server demonstrating write-heavy workloads.

## Usage Patterns for Agents

When generating code or analyzing issues:

1.  **Delegation**: Remember that `net.Conn` must be delegated to `gaio` via `watcher.Read` or `watcher.Write`.
2.  **Completion**: Results are retrieved via `watcher.WaitIO()`. This is a blocking call that returns `OpResult`.
3.  **Buffers**:
    - **Read**: Can use a nil buffer to let `gaio` use its internal swap buffer.
    - **Write**: Must provide a pre-filled buffer.
4.  **Error Handling**: Check `OpResult.Error`. `ErrDeadline` indicates a timeout.

## Common Tasks

- **Adding a new platform**: Implement a new `aio_<platform>.go` file adhering to the `poller` interface.
- **Optimizing performance**: Look into `watcher.go`'s loop and buffer management.
- **Debugging**: Use `examples/echo-server` to reproduce issues.

