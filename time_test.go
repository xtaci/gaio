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
	"container/heap"
	"testing"
	"time"
)

func TestTimedHeapOrderAndIndex(t *testing.T) {
	var h timedHeap
	heap.Push(&h, &aiocb{deadline: time.Unix(10, 0)})
	heap.Push(&h, &aiocb{deadline: time.Unix(5, 0)})
	heap.Push(&h, &aiocb{deadline: time.Unix(7, 0)})

	for i, cb := range h {
		if cb.idx != i {
			t.Fatalf("idx not updated: want %d got %d", i, cb.idx)
		}
	}

	pop1 := heap.Pop(&h).(*aiocb)
	pop2 := heap.Pop(&h).(*aiocb)
	pop3 := heap.Pop(&h).(*aiocb)

	if !pop1.deadline.Before(pop2.deadline) || !pop2.deadline.Before(pop3.deadline) {
		t.Fatal("timedHeap did not pop in ascending deadline order")
	}
}

func TestTimedHeapSwapUpdatesIndex(t *testing.T) {
	a := &aiocb{idx: 0}
	b := &aiocb{idx: 1}
	h := timedHeap{a, b}

	h.Swap(0, 1)
	if a.idx != 1 || b.idx != 0 {
		t.Fatalf("Swap did not update idx: a=%d b=%d", a.idx, b.idx)
	}
}
