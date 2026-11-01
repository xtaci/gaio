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

// timedHeap orders requests by deadline, with the earliest deadline at the root.
type timedHeap []*aiocb

// Len returns the number of elements in the heap.
func (h timedHeap) Len() int {
	return len(h)
}

// Less reports whether the request at i has an earlier deadline than the one at j.
func (h timedHeap) Less(i, j int) bool {
	return h[i].deadline.Before(h[j].deadline)
}

// Swap swaps the elements with indexes i and j.
func (h timedHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].idx = i
	h[j].idx = j
}

// Push adds a request and records its heap index.
func (h *timedHeap) Push(x any) {
	cb := x.(*aiocb)
	cb.idx = len(*h)
	*h = append(*h, cb)
}

// Pop removes and returns the last element in the backing slice. container/heap
// moves the earliest-deadline element there before calling this method.
func (h *timedHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	old[n-1] = nil // Avoid memory leak
	*h = old[:n-1]
	return x
}
