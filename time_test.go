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
	"testing"
	"time"
)

func TestTimedHeapPopsByDeadlineAndMaintainsIndices(t *testing.T) {
	h := newTimedHeap()
	h.Push(&aiocb{deadline: time.Unix(10, 0)})
	h.Push(&aiocb{deadline: time.Unix(5, 0)})
	h.Push(&aiocb{deadline: time.Unix(7, 0)})

	for i, cb := range h.items {
		if cb.idx != i {
			t.Fatalf("idx not updated: want %d got %d", i, cb.idx)
		}
	}

	pop1 := h.Pop()
	pop2 := h.Pop()
	pop3 := h.Pop()

	if !pop1.deadline.Before(pop2.deadline) || !pop2.deadline.Before(pop3.deadline) {
		t.Fatal("timedHeap did not pop in ascending deadline order")
	}
	if pop1.idx != -1 || pop2.idx != -1 || pop3.idx != -1 {
		t.Fatal("popped requests should not have a heap index")
	}
}

func TestTimedHeapSwapUpdatesIndices(t *testing.T) {
	a := &aiocb{idx: 0}
	b := &aiocb{idx: 1}
	h := newTimedHeap()
	h.items = []*aiocb{a, b}

	h.Swap(0, 1)
	if a.idx != 1 || b.idx != 0 {
		t.Fatalf("Swap did not update idx: a=%d b=%d", a.idx, b.idx)
	}
}

func TestTimedHeapRemoveMaintainsOrderAndIndices(t *testing.T) {
	h := newTimedHeap()
	requests := []*aiocb{
		{deadline: time.Unix(10, 0)},
		{deadline: time.Unix(5, 0)},
		{deadline: time.Unix(7, 0)},
		{deadline: time.Unix(3, 0)},
	}
	for _, cb := range requests {
		h.Push(cb)
	}

	removed := h.Remove(requests[3].idx)
	if removed != requests[3] || removed.idx != -1 {
		t.Fatal("Remove did not return and invalidate the requested entry")
	}
	for i, cb := range h.items {
		if cb.idx != i {
			t.Fatalf("idx not updated after Remove: want %d got %d", i, cb.idx)
		}
	}
	if got := h.Pop(); got != requests[1] {
		t.Fatalf("earliest request after Remove: want %p got %p", requests[1], got)
	}
}

func TestTimedHeapRemoveSiftsUpAndMaintainsIndices(t *testing.T) {
	h := newTimedHeap()
	requests := []*aiocb{
		{deadline: time.Unix(1, 0)},
		{deadline: time.Unix(10, 0)},
		{deadline: time.Unix(2, 0)},
		{deadline: time.Unix(11, 0)},
		{deadline: time.Unix(12, 0)},
		{deadline: time.Unix(3, 0)},
		{deadline: time.Unix(5, 0)},
	}
	for _, cb := range requests {
		h.Push(cb)
	}

	removed := h.Remove(requests[3].idx)
	if removed != requests[3] || removed.idx != -1 {
		t.Fatal("Remove did not return and invalidate the requested entry")
	}
	for i, cb := range h.items {
		if cb.idx != i {
			t.Fatalf("idx not updated after sift-up: want %d got %d", i, cb.idx)
		}
		if i > 0 && h.items[(i-1)/2].deadline.After(cb.deadline) {
			t.Fatalf("heap order broken after sift-up at index %d", i)
		}
	}
}
