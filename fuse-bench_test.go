package main

import (
	"fmt"
	"testing"

	go2fuse "github.com/cpuguy83/go2fuse"
	gofusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// BenchmarkFUSEMount measures the cost of go2fuse.Mount + server.Unmount for a [LayeredFS] of
// varying depth.  This is the FUSE-specific overhead tarfs pays on every View/Prepare that
// the overlay snapshotter does not: the kernel mount(2) handshake and the serve goroutine
// setup, not including any actual file reads through the mount.
//
// Requires FUSE in the environment; skips otherwise (same guard as TestFUSEMount_singleLayer).
// Run under unshare to enable FUSE mounting without root:
//
//	go test -bench=BenchmarkFUSEMount -exec "unshare --user --map-root-user --mount" ./...
func BenchmarkFUSEMount(b *testing.B) {
	checkFUSE(b)

	for _, depth := range []int{1, 5, 10, 20} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			layers := buildReadFileStack(b, depth, "bench content\n")
			lfs := &LayeredFS{layers: layers}
			mountDir := b.TempDir()

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				server, err := go2fuse.Mount(mountDir, lfs, &gofusefs.Options{
					MountOptions: fuse.MountOptions{
						DirectMount: true,
					},
				})
				if err != nil {
					b.Skipf("FUSE mount failed (likely insufficient privileges): %v", err)
				}
				if err := server.Unmount(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
