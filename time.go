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

// minHeap is a typed min-heap. less defines priority, while setIndex keeps each
// item aware of its current position so it can be removed in O(log n).
type minHeap[T any] struct {
	items    []T
	less     func(T, T) bool
	setIndex func(T, int)
}

// newMinHeap creates an empty heap with the given ordering and index callbacks.
func newMinHeap[T any](less func(T, T) bool, setIndex func(T, int)) minHeap[T] {
	return minHeap[T]{less: less, setIndex: setIndex}
}

// Len returns the number of items in h.
func (h *minHeap[T]) Len() int {
	return len(h.items)
}

// Peek returns the minimum item, or the zero value and false when h is empty.
func (h *minHeap[T]) Peek() (T, bool) {
	if len(h.items) == 0 {
		var zero T
		return zero, false
	}
	return h.items[0], true
}

// Push adds item and restores the heap ordering.
func (h *minHeap[T]) Push(item T) {
	h.items = append(h.items, item)
	h.setIndex(item, len(h.items)-1)
	h.siftUp(len(h.items) - 1)
}

// Pop removes and returns the minimum item. It panics when h is empty.
func (h *minHeap[T]) Pop() T {
	return h.Remove(0)
}

// Remove removes and returns the item at index, restoring heap order and
// setting the removed item's index to -1. It panics for an invalid index.
func (h *minHeap[T]) Remove(index int) T {
	if index < 0 || index >= len(h.items) {
		panic("heap index out of range")
	}

	last := len(h.items) - 1
	removed := h.items[index]
	if index == last {
		var zero T
		h.items[last] = zero
		h.items = h.items[:last]
		h.setIndex(removed, -1)
		return removed
	}

	replacement := h.items[last]
	var zero T
	h.items[last] = zero
	h.items = h.items[:last]
	h.items[index] = replacement
	h.setIndex(replacement, index)
	if index > 0 && h.less(replacement, h.items[(index-1)/2]) {
		h.siftUp(index)
	} else {
		h.siftDown(index)
	}
	h.setIndex(removed, -1)
	return removed
}

// Swap exchanges two items and updates both recorded indices.
func (h *minHeap[T]) Swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.setIndex(h.items[i], i)
	h.setIndex(h.items[j], j)
}

// siftUp moves an item toward the root while it has higher priority.
func (h *minHeap[T]) siftUp(index int) {
	for index > 0 {
		parent := (index - 1) / 2
		if !h.less(h.items[index], h.items[parent]) {
			return
		}
		h.Swap(index, parent)
		index = parent
	}
}

// siftDown moves an item toward a leaf until both children have lower priority.
func (h *minHeap[T]) siftDown(index int) {
	for {
		left := 2*index + 1
		if left >= len(h.items) {
			return
		}
		smallest := left
		right := left + 1
		if right < len(h.items) && h.less(h.items[right], h.items[left]) {
			smallest = right
		}
		if !h.less(h.items[smallest], h.items[index]) {
			return
		}
		h.Swap(index, smallest)
		index = smallest
	}
}

// timedHeap orders requests by deadline, with the earliest deadline at the root.
type timedHeap struct {
	minHeap[*aiocb]
}

// newTimedHeap orders I/O requests by their deadlines.
func newTimedHeap() timedHeap {
	return timedHeap{minHeap: newMinHeap(
		func(a, b *aiocb) bool { return a.deadline.Before(b.deadline) },
		func(cb *aiocb, index int) { cb.idx = index },
	)}
}
