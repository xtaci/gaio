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

import (
	"runtime"
	"testing"
)

func TestSetAffinityInvalidCPU(t *testing.T) {
	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := w.SetPollerAffinity(-1); err != ErrCPUID {
		t.Fatalf("SetPollerAffinity(-1) expected ErrCPUID, got %v", err)
	}
	if err := w.SetLoopAffinity(-1); err != ErrCPUID {
		t.Fatalf("SetLoopAffinity(-1) expected ErrCPUID, got %v", err)
	}

	invalidCPU := runtime.NumCPU()
	if err := w.SetPollerAffinity(invalidCPU); err != ErrCPUID {
		t.Fatalf("SetPollerAffinity(%d) expected ErrCPUID, got %v", invalidCPU, err)
	}
	if err := w.SetLoopAffinity(invalidCPU); err != ErrCPUID {
		t.Fatalf("SetLoopAffinity(%d) expected ErrCPUID, got %v", invalidCPU, err)
	}
}

func TestSetLoopAffinityAfterClose(t *testing.T) {
	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if err := w.SetLoopAffinity(0); err != ErrConnClosed {
		t.Fatalf("SetLoopAffinity after Close expected ErrConnClosed, got %v", err)
	}
}
