package gaio_test

import (
	"math/rand"
	"net"
	"testing"

	"github.com/xtaci/gaio"
)

type detachCtx struct {
	id    int
	cmd   [2]byte
	times int
	conn  net.Conn
}

func newDetachCtx() *detachCtx {
	return &detachCtx{
		cmd: [2]byte{'0', '0'},
	}
}

// detachServer responses to character cmd received from connection,
// prints stored cmd or detach the connection and re-watch.
func detachServer(t testing.TB) net.Listener {
	addr, _ := net.ResolveTCPAddr("tcp", "localhost:0")
	ln, err := net.ListenTCP("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}

	w, err := gaio.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		for {
			results, err := w.WaitIO()
			if err != nil {
				return
			}

			for _, res := range results {
				switch res.Operation {
				case gaio.OpRead:
					if res.Error != nil {
						t.Log(res.Error)
						_ = w.Free(res.Conn)
						continue
					}
					ctx := res.Context.(*detachCtx)
					if res.Size > 0 {
						switch res.Buffer[0] {
						case 'p': // print current cmd
							_ = w.Write(ctx, res.Conn, ctx.cmd[:1])
						case 'x': // exit
							continue
						default: // remember the cmd and detach
							ctx.cmd[0] = res.Buffer[0]
							_ = w.Detach(ctx, res.Conn)
						}
					}
				case gaio.OpWrite:
					if res.Error != nil {
						t.Fatal(res.Error)
					}
					if res.Size > 0 {
						// write complete, start read again
						_ = w.Read(res.Context, res.Conn, nil)
					}
				case gaio.OpDetach:
					if res.Error != nil {
						t.Fatal(res.Error)
					}
					ctx := res.Context.(*detachCtx)
					// conn in detach result should be the old one
					if res.Conn != ctx.conn {
						t.Error("conn changed", res.Conn, ctx.conn)
					}
					conn, err := gaio.RecoverConnFromDetachResult(res)
					if err != nil {
						t.Fatal(err)
					}
					ctx.conn = conn
					// join the watcher again
					_ = w.Write(ctx, conn, ctx.cmd[:1])
				}
			}
		}
	}()

	go func() {
		for {
			conn, err := ln.AcceptTCP()
			if err != nil {
				_ = w.Close()
				return
			}
			ctx := newDetachCtx()
			ctx.conn = conn
			err = w.Read(ctx, conn, nil)
			if err != nil {
				t.Fatal(err)
			}
		}
	}()
	return ln
}

func testDetachParallel(t *testing.T, par int, times int) {
	t.Log("testing concurrent:", par, "connections")
	ln := detachServer(t)
	defer func() { _ = ln.Close() }()

	w, err := gaio.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	go func() {
		for i := 0; i < par; i++ {
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			ctx := newDetachCtx()
			ctx.id = i
			ctx.cmd[0] = 'p' // print init cmd
			err = w.Write(ctx, conn, ctx.cmd[:1])
			if err != nil {
				t.Fatal(err)
			}
		}
	}()

	done := 0
	for done < par {
		results, err := w.WaitIO()
		if err != nil {
			t.Log(err)
			return
		}

		for _, res := range results {
			switch res.Operation {
			case gaio.OpWrite:
				// recv
				if res.Error != nil {
					t.Error(res.Error)
				}
				ctx := res.Context.(*detachCtx)
				if ctx.cmd[0] == 'x' {
					err = w.Free(res.Conn)
					done++
				} else {
					err = w.Read(ctx, res.Conn, nil)
				}
				if err != nil {
					t.Fatal(err)
				}
			case gaio.OpRead:
				if res.Error != nil {
					t.Error(res.Error)
				}
				ctx := res.Context.(*detachCtx)
				if ctx.cmd[1] != res.Buffer[0] {
					t.Error("not match", ctx.id, string(ctx.cmd[0]), string(ctx.cmd[1]), string(res.Buffer[0]))
				}
				ctx.times++
				if times > 0 && ctx.times > times {
					ctx.cmd[0] = 'x'
				} else {
					n := rand.Intn(10)
					if n > 5 {
						ctx.cmd[0] = 'p'
					} else {
						ctx.cmd[0] = byte(int('0') + n)
						ctx.cmd[1] = ctx.cmd[0]
					}
				}
				if err := w.Write(ctx, res.Conn, ctx.cmd[:1]); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	_ = w.Close()
}

func TestDetach10(t *testing.T) {
	testDetachParallel(t, 10, 100)
}

func TestDetach100(t *testing.T) {
	testDetachParallel(t, 100, 1000)
}
