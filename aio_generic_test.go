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

func TestRequestQueuePushRemoveAndInit(t *testing.T) {
	var q requestQueue[int]
	first := q.PushBack(1)
	middle := q.PushBack(2)
	last := q.PushBack(3)

	q.Remove(middle)
	if q.Len() != 2 || q.Front() != first || first.Next() != last {
		t.Fatal("removing a middle node did not preserve queue order")
	}
	if middle.Value() != 0 || middle.Next() != nil {
		t.Fatal("removed node retained its value or links")
	}

	q.Init()
	if q.Len() != 0 || q.Front() != nil || last.Value() != 0 {
		t.Fatal("Init did not clear the queue")
	}
}
