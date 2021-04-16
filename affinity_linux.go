// +build linux

package gaio

/*
#define _GNU_SOURCE
#include <sched.h>
#include <pthread.h>

void lock_thread(int cpuid) {
        pthread_t tid;
        cpu_set_t cpuset;

        tid = pthread_self();
        CPU_ZERO(&cpuset);
        CPU_SET(cpuid, &cpuset);
    pthread_setaffinity_np(tid, sizeof(cpu_set_t), &cpuset);
}
*/
import "C"
import (
	"crypto/rand"
	"runtime"
)

// bind thread & goroutine to a specific CPU
func setAffinity() {
	b := make([]byte, 1)
	rand.Read(b)
	runtime.LockOSThread()
	C.lock_thread(C.int(b[0]))
}
