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

// timedHeap is a min-heap that sorts aiocb elements by their deadline.
// It implements the heap.Interface from the container/heap package.
type timedHeap []*aiocb

// Len returns the number of elements in the heap.
func (h timedHeap) Len() int {
	return len(h)
}

// Less reports whether the element with index i should sort before the element with index j.
// It is used to order the elements by their deadline in ascending order.
func (h timedHeap) Less(i, j int) bool {
	return h[i].deadline.Before(h[j].deadline)
}

// Swap swaps the elements with indexes i and j.
func (h timedHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].idx = i
	h[j].idx = j
}

// Push adds an element to the heap.
// This method is used by the heap.Push function.
func (h *timedHeap) Push(x any) {
	cb := x.(*aiocb)
	cb.idx = len(*h)
	*h = append(*h, cb)
}

// Pop removes and returns the element with the highest priority (lowest deadline).
// This method is used by the heap.Pop function.
func (h *timedHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	old[n-1] = nil // Avoid memory leak
	*h = old[:n-1]
	return x
}
