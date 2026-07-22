package main

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io/fs"
	"testing"

	"github.com/jonjohnsonjr/targz/tarfs"
)

// makeLayerB builds a *tarfs.FS from name/body pairs.  It is the [testing.B] counterpart of makeTar in layers_test.go.
func makeLayerB(b *testing.B, entries []struct{ name, body string }) *tarfs.FS {
	b.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name: e.name,
			Mode: 0o644,
			Size: int64(len(e.body)),
		}
		if len(e.name) > 0 && e.name[len(e.name)-1] == '/' {
			hdr.Typeflag = tar.TypeDir
			hdr.Size = 0
			hdr.Mode = 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			b.Fatalf("WriteHeader %q: %v", e.name, err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				b.Fatalf("Write %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		b.Fatalf("tar close: %v", err)
	}
	data := buf.Bytes()
	fsys, err := tarfs.New(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		b.Fatalf("tarfs.New: %v", err)
	}
	return fsys
}

// buildReadFileStack builds a []fs.FS of the given depth for use in ReadFile benchmarks.
// layers[0] (topmost) contains "etc/hostname" and layers[depth-1] (base) contains "etc/os-release";
// intermediate layers carry padding files only, so neither target appears in them.
func buildReadFileStack(b *testing.B, depth int, fileBody string) []fs.FS {
	b.Helper()
	layers := make([]fs.FS, depth)

	if depth == 1 {
		layers[0] = makeLayerB(b, []struct{ name, body string }{
			{"etc/", ""},
			{"etc/hostname", fileBody},
			{"etc/os-release", fileBody},
		})
		return layers
	}

	layers[0] = makeLayerB(b, []struct{ name, body string }{
		{"etc/", ""},
		{"etc/hostname", fileBody},
	})

	for i := 1; i < depth-1; i++ {
		layers[i] = makeLayerB(b, []struct{ name, body string }{
			{"etc/", ""},
			{fmt.Sprintf("usr/lib/l%d/", i), ""},
			{fmt.Sprintf("usr/lib/l%d/data", i), fmt.Sprintf("l%d\n", i)},
		})
	}

	base := []struct{ name, body string }{
		{"etc/", ""},
		{"etc/os-release", fileBody},
	}
	for i := range 8 {
		base = append(base, struct{ name, body string }{
			fmt.Sprintf("lib/lib%02d.so", i), "elf placeholder\n",
		})
	}
	layers[depth-1] = makeLayerB(b, base)

	return layers
}

// buildReadDirStack builds a *LayeredFS of the given depth for use in ReadDir benchmarks.
// The base layer has 20 files in "etc/".  Each intermediate layer whiteouts whiteoutsPerLayer
// of those base files and adds 3 new ones.
func buildReadDirStack(b *testing.B, depth, whiteoutsPerLayer int) *LayeredFS {
	b.Helper()
	const baseCount = 20

	layers := make([]fs.FS, depth)

	base := make([]struct{ name, body string }, 0, baseCount+1)
	base = append(base, struct{ name, body string }{"etc/", ""})
	for i := range baseCount {
		base = append(base, struct{ name, body string }{
			fmt.Sprintf("etc/file%02d", i), fmt.Sprintf("content%d\n", i),
		})
	}
	layers[depth-1] = makeLayerB(b, base)

	if depth == 1 {
		return &LayeredFS{layers: layers}
	}

	layers[0] = makeLayerB(b, []struct{ name, body string }{
		{"etc/", ""},
		{"etc/app.conf", "app=prod\n"},
	})

	for i := 1; i < depth-1; i++ {
		entries := []struct{ name, body string }{{"etc/", ""}}
		for j := range whiteoutsPerLayer {
			entries = append(entries, struct{ name, body string }{
				fmt.Sprintf("etc/.wh.file%02d", j), "",
			})
		}
		for j := range 3 {
			entries = append(entries, struct{ name, body string }{
				fmt.Sprintf("etc/l%d-%d", i, j), fmt.Sprintf("l%d-%d\n", i, j),
			})
		}
		layers[i] = makeLayerB(b, entries)
	}

	return &LayeredFS{layers: layers}
}

// BenchmarkLayeredFS_ReadFile measures Open+Read cost for a regular file through a stacked
// [LayeredFS].  Sub-benchmarks vary stack depth and whether the target file is in the topmost
// layer (found immediately) or the deepest layer (all layers traversed).
func BenchmarkLayeredFS_ReadFile(b *testing.B) {
	const fileBody = "hello from the base layer -- content used to measure read throughput\n"

	for _, depth := range []int{1, 5, 10, 20} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			layers := buildReadFileStack(b, depth, fileBody)
			lfs := &LayeredFS{layers: layers}

			b.Run("topLayer", func(b *testing.B) {
				b.SetBytes(int64(len(fileBody)))
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					data, err := fs.ReadFile(lfs, "etc/hostname")
					if err != nil {
						b.Fatal(err)
					}
					_ = data
				}
			})

			b.Run("bottomLayer", func(b *testing.B) {
				b.SetBytes(int64(len(fileBody)))
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					data, err := fs.ReadFile(lfs, "etc/os-release")
					if err != nil {
						b.Fatal(err)
					}
					_ = data
				}
			})
		})
	}
}

// BenchmarkLayeredFS_ReadDir measures ReadDir cost through a stacked [LayeredFS].
// Sub-benchmarks vary stack depth and the number of whiteout entries per intermediate layer.
// The "entries/op" metric shows how many visible entries each ReadDir returns.
func BenchmarkLayeredFS_ReadDir(b *testing.B) {
	type config struct {
		depth     int
		whiteouts int
	}
	configs := []config{
		{1, 0},
		{5, 0},
		{5, 4},
		{5, 16},
		{10, 0},
		{10, 4},
		{10, 16},
		{20, 0},
		{20, 4},
	}

	for _, c := range configs {
		b.Run(fmt.Sprintf("depth=%d/whiteouts=%d", c.depth, c.whiteouts), func(b *testing.B) {
			lfs := buildReadDirStack(b, c.depth, c.whiteouts)

			setup, err := fs.ReadDir(lfs, "etc")
			if err != nil {
				b.Fatalf("ReadDir setup: %v", err)
			}
			entryCount := len(setup)

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				got, err := fs.ReadDir(lfs, "etc")
				if err != nil {
					b.Fatal(err)
				}
				_ = got
			}
			// called after the loop so b.result.Extra is not reset by b.Loop's first-iteration initialization
			b.ReportMetric(float64(entryCount), "entries/op")
		})
	}
}
