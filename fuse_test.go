package main

import (
	"io/fs"
	"os"
	"os/exec"
	"testing"
	"time"

	go2fuse "github.com/cpuguy83/go2fuse"
	gofusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// checkFUSE skips the test if FUSE mounting is not available in this environment (no fusermount/fusermount3, or insufficient privileges).
func checkFUSE(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("fusermount3"); err != nil {
		if _, err := exec.LookPath("fusermount"); err != nil {
			t.Skip("FUSE not available: neither fusermount nor fusermount3 found (install fuse3 package)")
		}
	}
}

func TestFUSEMount_singleLayer(t *testing.T) {
	checkFUSE(t)

	layer := makeTar(t, []struct{ name, body string }{
		{"etc/", ""},
		{"etc/hostname", "fusehost"},
		{"usr/", ""},
		{"usr/bin/", ""},
		{"usr/bin/hello", "#!/bin/sh\necho hello\n"},
	})

	mnt := t.TempDir()
	// DirectMount uses syscall.Mount directly (no fusermount3 subprocess),
	// which avoids the go-fuse env-stripping issue where it only passes
	// _FUSE_COMMFD=3 to fusermount3 (dropping $USER).
	// Falls back to fusermount3 if syscall.Mount fails.
	server, err := go2fuse.Mount(mnt, layer, &gofusefs.Options{
		MountOptions: fuse.MountOptions{
			FsName:      "tarfs-test",
			Name:        "tarfs-test",
			DirectMount: true,
		},
	})
	if err != nil {
		t.Skipf("FUSE mount failed (likely insufficient privileges in this environment): %v", err)
	}
	defer server.Unmount()

	// give the FUSE server a moment to start
	time.Sleep(100 * time.Millisecond)

	// read a file through the FUSE mount
	data, err := os.ReadFile(mnt + "/etc/hostname")
	if err != nil {
		t.Fatalf("ReadFile through FUSE: %v", err)
	}
	if string(data) != "fusehost" {
		t.Errorf("etc/hostname = %q, want %q", data, "fusehost")
	}

	// readdir
	entries, err := os.ReadDir(mnt + "/usr/bin")
	if err != nil {
		t.Fatalf("ReadDir through FUSE: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "hello" {
		t.Errorf("usr/bin entries = %v, want [hello]", entries)
	}

	// stat
	info, err := os.Stat(mnt + "/usr/bin/hello")
	if err != nil {
		t.Fatalf("Stat through FUSE: %v", err)
	}
	if info.Mode()&fs.ModeType != 0 {
		t.Errorf("hello should be a regular file, got mode %v", info.Mode())
	}
}
