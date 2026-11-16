// A planted js/wasm program for sidecarwasm_boot.test.ts (QD-909): what a long CPU-bound call, an MLS commit over a
// large group or decrypting a large repository, does to Go's runtime in a Worker. It builds a 4 MiB heap of pointers,
// which every GC cycle must mark, then allocates 256 MiB of garbage in 64 KiB blocks in a loop that never blocks.
//
// js/wasm has one thread and no preemption, so the background mark worker runs only when the program blocks, and this
// one never does. By default a cycle then marks only in the assists its allocations pay, which fall far behind
// blocks this large, and every block allocated while it stays open is marked live: linear memory, which never shrinks,
// ends past 200 MB. With GODEBUG=gcstoptheworld=1, the environment the page Worker's loader gives sidecar.wasm, each
// cycle marks to its end before the loop goes on, and the program needs about 12 MB. sidecar's legs_test.go holds the
// same program in its Worker harness (workerGcProgram).
//
// It lives under testdata so that the js module's ./... never holds it.
package main

type node struct {
	next  *node
	links [7]*node
}

var live *node

var garbage []byte

func main() {
	for i := 0; i < 64*1024; i += 1 {
		live = &node{next: live}
	}
	for i := 0; i < 4*1024; i += 1 {
		garbage = make([]byte, 64*1024)
	}
}
