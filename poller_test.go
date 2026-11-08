// The MIT License (MIT)
//
// Copyright (c) 2019 xtaci
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package gaio

import "testing"

// TestPollerCloseAfterWaitExit covers the window between closing the die
// channel and waking the poll loop in Close. If Wait observes die first, it
// exits and releases the wakeup descriptor, and the subsequent wakeup must not
// turn Close into a failure: the poller is closed, which is what Close asked
// for.
func TestPollerCloseAfterWaitExit(t *testing.T) {
	p, err := openPoll()
	if err != nil {
		t.Fatal(err)
	}

	waitDone := make(chan struct{})
	go func() {
		p.Wait(make(chan Signal))
		close(waitDone)
	}()

	// Stop the poll loop without waking it, exactly as the race would.
	p.dieOnce.Do(func() { close(p.die) })
	<-waitDone

	if err := p.Close(); err != nil {
		t.Fatalf("Close after Wait exited: %v", err)
	}
}
