package main

import (
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"
)

// LayeredFS merges multiple fs.FS layers into a single read-only filesystem, applying OCI image layer whiteout semantics.  layers[0] is the topmost (most recently added) layer; layers[len-1] is the base.
type LayeredFS struct {
	layers []fs.FS
}

// layerFS is satisfied by tarfs.FS (and any other layer that supports lstat and readlink without following symlinks).
type layerFS interface {
	fs.FS
	Lstat(name string) (fs.FileInfo, error)
	ReadLink(name string) (string, error)
}

// Open implements fs.FS.  It finds the topmost non-whited-out entry for name across all layers.  Directories are returned as layeredDir values so that their ReadDir spans all layers.
func (l *LayeredFS) Open(name string) (fs.File, error) {
	if name == "." {
		return &layeredDir{l: l, path: "."}, nil
	}

	dir := path.Dir(name)
	base := path.Base(name)

	for i, layer := range l.layers {
		lfs, ok := layer.(layerFS)
		if !ok {
			// fall back to plain Open with no whiteout check for this layer
			if f, err := layer.Open(name); err == nil {
				return f, nil
			}
			continue
		}

		// explicit whiteout for our target
		if _, err := lfs.Lstat(path.Join(dir, ".wh."+base)); err == nil {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		}

		// entry present in this layer
		if info, err := lfs.Lstat(name); err == nil {
			if info.IsDir() {
				return &layeredDir{l: l, path: name}, nil
			}
			return layer.Open(name)
		}

		// opaque whiteout: this layer erases all lower-layer content of dir
		if i < len(l.layers)-1 {
			if _, err := lfs.Lstat(path.Join(dir, ".wh..wh..opq")); err == nil {
				return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
			}
		}
	}

	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// Stat implements fs.StatFS.
func (l *LayeredFS) Stat(name string) (fs.FileInfo, error) {
	if name == "." {
		return syntheticDir("."), nil
	}
	f, err := l.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}

// Lstat returns FileInfo for name without following symlinks, satisfying the readLinkFS interface expected by cpuguy83/go2fuse.
func (l *LayeredFS) Lstat(name string) (fs.FileInfo, error) {
	if name == "." {
		return syntheticDir("."), nil
	}
	dir := path.Dir(name)
	base := path.Base(name)

	for i, layer := range l.layers {
		lfs, ok := layer.(layerFS)
		if !ok {
			continue
		}

		// explicit whiteout
		if _, err := lfs.Lstat(path.Join(dir, ".wh."+base)); err == nil {
			return nil, &fs.PathError{Op: "lstat", Path: name, Err: fs.ErrNotExist}
		}

		// entry present
		if info, err := lfs.Lstat(name); err == nil {
			return info, nil
		}

		// opaque whiteout
		if i < len(l.layers)-1 {
			if _, err := lfs.Lstat(path.Join(dir, ".wh..wh..opq")); err == nil {
				return nil, &fs.PathError{Op: "lstat", Path: name, Err: fs.ErrNotExist}
			}
		}
	}

	return nil, &fs.PathError{Op: "lstat", Path: name, Err: fs.ErrNotExist}
}

// ReadLink returns the target of the named symlink, satisfying the readLinkFS interface expected by cpuguy83/go2fuse.
func (l *LayeredFS) ReadLink(name string) (string, error) {
	dir := path.Dir(name)
	base := path.Base(name)

	for i, layer := range l.layers {
		lfs, ok := layer.(layerFS)
		if !ok {
			continue
		}

		// explicit whiteout
		if _, err := lfs.Lstat(path.Join(dir, ".wh."+base)); err == nil {
			return "", &fs.PathError{Op: "readlink", Path: name, Err: fs.ErrNotExist}
		}

		// symlink present
		if target, err := lfs.ReadLink(name); err == nil {
			return target, nil
		}

		// opaque whiteout
		if i < len(l.layers)-1 {
			if _, err := lfs.Lstat(path.Join(dir, ".wh..wh..opq")); err == nil {
				return "", &fs.PathError{Op: "readlink", Path: name, Err: fs.ErrNotExist}
			}
		}
	}

	return "", &fs.PathError{Op: "readlink", Path: name, Err: fs.ErrNotExist}
}

// ReadDir implements fs.ReadDirFS.
func (l *LayeredFS) ReadDir(name string) ([]fs.DirEntry, error) {
	seen := map[string]fs.DirEntry{}
	whited := map[string]bool{}

	for _, layer := range l.layers {
		entries, err := fs.ReadDir(layer, name)
		if err != nil {
			// layer may not contain this directory; that's fine
			continue
		}

		opaque := false
		for _, e := range entries {
			n := e.Name()
			if n == ".wh..wh..opq" {
				opaque = true
				continue
			}
			if strings.HasPrefix(n, ".wh.") {
				whited[strings.TrimPrefix(n, ".wh.")] = true
				continue
			}
			if _, alreadySeen := seen[n]; !alreadySeen && !whited[n] {
				seen[n] = e
			}
		}

		if opaque {
			break
		}
	}

	result := make([]fs.DirEntry, 0, len(seen))
	for _, e := range seen {
		result = append(result, e)
	}
	slices.SortFunc(result, func(a, b fs.DirEntry) int {
		if a.Name() < b.Name() {
			return -1
		}
		if a.Name() > b.Name() {
			return 1
		}
		return 0
	})
	return result, nil
}

// layeredDir implements fs.File for directory entries spanning all layers.
type layeredDir struct {
	l    *LayeredFS
	path string
}

func (d *layeredDir) Stat() (fs.FileInfo, error) {
	for _, layer := range d.l.layers {
		if lfs, ok := layer.(layerFS); ok {
			if info, err := lfs.Lstat(d.path); err == nil && info.IsDir() {
				return info, nil
			}
		}
	}
	return syntheticDir(d.path), nil
}

func (d *layeredDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.path, Err: fs.ErrInvalid}
}

func (d *layeredDir) Close() error { return nil }

func (d *layeredDir) ReadDir(n int) ([]fs.DirEntry, error) {
	all, err := d.l.ReadDir(d.path)
	if err != nil {
		return nil, err
	}
	if n <= 0 || n >= len(all) {
		return all, nil
	}
	return all[:n], nil
}

// syntheticDirInfo is a minimal fs.FileInfo for directories with no real entry.
type syntheticDirInfo string

func (s syntheticDirInfo) Name() string       { return path.Base(string(s)) }
func (s syntheticDirInfo) Size() int64        { return 0 }
func (s syntheticDirInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o555 }
func (s syntheticDirInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (s syntheticDirInfo) IsDir() bool        { return true }
func (s syntheticDirInfo) Sys() any           { return nil }

func syntheticDir(p string) fs.FileInfo { return syntheticDirInfo(p) }
