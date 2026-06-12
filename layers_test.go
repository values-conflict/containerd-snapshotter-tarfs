package main

import (
	"archive/tar"
	"bytes"
	"io/fs"
	"testing"

	"github.com/jonjohnsonjr/targz/tarfs"
)

// makeTar builds a tar blob from a slice of (name, content) pairs.  names starting with "/" are stripped; directories end with "/".
func makeTar(t *testing.T, entries []struct{ name, body string }) *tarfs.FS {
	t.Helper()
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
			t.Fatalf("WriteHeader %q: %v", e.name, err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("Write %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data := buf.Bytes()
	fsys, err := tarfs.New(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("tarfs.New: %v", err)
	}
	return fsys
}

func readFile(t *testing.T, fsys fs.FS, name string) string {
	t.Helper()
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", name, err)
	}
	return string(data)
}

func assertNotExist(t *testing.T, fsys fs.FS, name string) {
	t.Helper()
	_, err := fsys.Open(name)
	if err == nil {
		t.Errorf("Open(%q): expected error, got nil", name)
	}
}

func TestLayeredFS_basicMerge(t *testing.T) {
	base := makeTar(t, []struct{ name, body string }{
		{"etc/", ""},
		{"etc/hostname", "base-host"},
		{"etc/os-release", "ID=base"},
	})
	top := makeTar(t, []struct{ name, body string }{
		{"etc/", ""},
		{"etc/hostname", "top-host"}, // overrides base
		{"usr/", ""},
		{"usr/bin/", ""},
	})

	lfs := &LayeredFS{layers: []fs.FS{top, base}}

	if got := readFile(t, lfs, "etc/hostname"); got != "top-host" {
		t.Errorf("etc/hostname = %q, want %q", got, "top-host")
	}
	if got := readFile(t, lfs, "etc/os-release"); got != "ID=base" {
		t.Errorf("etc/os-release = %q, want %q", got, "ID=base")
	}
}

func TestLayeredFS_explicitWhiteout(t *testing.T) {
	base := makeTar(t, []struct{ name, body string }{
		{"etc/", ""},
		{"etc/secret", "hunter2"},
		{"etc/keep", "yes"},
	})
	top := makeTar(t, []struct{ name, body string }{
		{"etc/", ""},
		{".wh.etc", ""},        // this would whiteout /etc itself
		{"etc/.wh.secret", ""}, // whiteout etc/secret
	})

	// layers = [top, base]: top is most recent
	lfs := &LayeredFS{layers: []fs.FS{top, base}}

	assertNotExist(t, lfs, "etc/secret")
	if got := readFile(t, lfs, "etc/keep"); got != "yes" {
		t.Errorf("etc/keep = %q, want %q", got, "yes")
	}
}

func TestLayeredFS_opaqueWhiteout(t *testing.T) {
	base := makeTar(t, []struct{ name, body string }{
		{"lib/", ""},
		{"lib/old.so", "old"},
		{"lib/shared.so", "shared-base"},
	})
	top := makeTar(t, []struct{ name, body string }{
		{"lib/", ""},
		{"lib/.wh..wh..opq", ""}, // opaque: hides entire lib/ from base
		{"lib/new.so", "new"},
		{"lib/shared.so", "shared-top"},
	})

	lfs := &LayeredFS{layers: []fs.FS{top, base}}

	assertNotExist(t, lfs, "lib/old.so")
	if got := readFile(t, lfs, "lib/new.so"); got != "new" {
		t.Errorf("lib/new.so = %q, want %q", got, "new")
	}
	if got := readFile(t, lfs, "lib/shared.so"); got != "shared-top" {
		t.Errorf("lib/shared.so = %q, want %q", got, "shared-top")
	}
}

func TestLayeredFS_readDir(t *testing.T) {
	base := makeTar(t, []struct{ name, body string }{
		{"dir/", ""},
		{"dir/a", "a"},
		{"dir/b", "b"},
	})
	top := makeTar(t, []struct{ name, body string }{
		{"dir/", ""},
		{"dir/.wh.a", ""},
		{"dir/c", "c"},
	})

	lfs := &LayeredFS{layers: []fs.FS{top, base}}

	entries, err := fs.ReadDir(lfs, "dir")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}

	// expect: b and c (a is whited out)
	want := []string{"b", "c"}
	if len(names) != len(want) {
		t.Fatalf("ReadDir = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("ReadDir[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}
