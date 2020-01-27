package gaio

// a heap for sorted timeout
type timedHeap []*aiocb

func (h timedHeap) Len() int           { return len(h) }
func (h timedHeap) Less(i, j int) bool { return h[i].deadline.Before(h[j].deadline) }
func (h timedHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].idx = i
	h[j].idx = j
}

func (h *timedHeap) Push(x interface{}) {
	*h = append(*h, x.(*aiocb))
	n := len(*h)
	(*h)[n-1].idx = n - 1
}
func (h *timedHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	old[n-1] = nil // avoid memory leak
	*h = old[0 : n-1]
	return x
}
