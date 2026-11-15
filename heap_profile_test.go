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
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"testing"
	"unsafe"
)

func TestHeapProfileQueueStorage(t *testing.T) {
	if os.Getenv("PROFILE_CONNECTIONS") == "" || os.Getenv("PROFILE_REQUESTS") == "" {
		t.Skip("set PROFILE_CONNECTIONS and PROFILE_REQUESTS to run the heap profile")
	}

	connectionCount := profileEnvInt(t, "PROFILE_CONNECTIONS")
	requestsPerConn := profileEnvInt(t, "PROFILE_REQUESTS")
	profilePath := os.Getenv("PROFILE_FILE")
	previousMemProfileRate := runtime.MemProfileRate
	runtime.MemProfileRate = 1
	defer func() {
		runtime.MemProfileRate = previousMemProfileRate
	}()

	runtime.GC()
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)

	descs := make([]*fdDesc, connectionCount)
	for i := range descs {
		desc := new(fdDesc)
		for j := 0; j < requestsPerConn; j++ {
			desc.readers.PushBack(new(aiocb))
		}
		descs[i] = desc
	}

	runtime.GC()
	var current runtime.MemStats
	runtime.ReadMemStats(&current)
	runtime.KeepAlive(descs)

	fmt.Printf(
		"profile connections=%d requests=%d heap_delta=%d object_delta=%d desc_size=%d queue_size=%d node_size=%d request_size=%d\n",
		connectionCount,
		connectionCount*requestsPerConn,
		int64(current.HeapAlloc)-int64(baseline.HeapAlloc),
		int64(current.HeapObjects)-int64(baseline.HeapObjects),
		unsafe.Sizeof(fdDesc{}),
		unsafe.Sizeof(requestQueue[*aiocb]{}),
		unsafe.Sizeof(requestNode[*aiocb]{}),
		unsafe.Sizeof(aiocb{}),
	)

	if profilePath != "" {
		file, err := os.Create(profilePath)
		if err != nil {
			t.Fatal(err)
		}
		if err := pprof.WriteHeapProfile(file); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func profileEnvInt(t *testing.T, name string) int {
	t.Helper()
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value <= 0 {
		t.Fatalf("%s must be a positive integer", name)
	}
	return value
}
