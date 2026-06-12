package main

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"io/fs"
	"strconv"
	"sync"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/plugins/content/local"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// memLabelStore is a simple in-memory label store for use in tests.  It satisfies local.LabelStore.
type memLabelStore struct {
	mu     sync.Mutex
	labels map[digest.Digest]map[string]string
}

func newMemLabelStore() *memLabelStore {
	return &memLabelStore{labels: map[digest.Digest]map[string]string{}}
}

func (m *memLabelStore) Get(d digest.Digest) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l, ok := m.labels[d]; ok {
		cp := make(map[string]string, len(l))
		for k, v := range l {
			cp[k] = v
		}
		return cp, nil
	}
	return map[string]string{}, nil
}

func (m *memLabelStore) Set(d digest.Digest, labels map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.labels[d] = labels
	return nil
}

func (m *memLabelStore) Update(d digest.Digest, updates map[string]string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.labels[d] == nil {
		m.labels[d] = map[string]string{}
	}
	for k, v := range updates {
		if v == "" {
			delete(m.labels[d], k)
		} else {
			m.labels[d][k] = v
		}
	}
	cp := make(map[string]string, len(m.labels[d]))
	for k, v := range m.labels[d] {
		cp[k] = v
	}
	return cp, nil
}

// newTestStore creates a local content store backed by an in-memory label store.
func newTestStore(t *testing.T) content.Store {
	t.Helper()
	cs, err := local.NewLabeledStore(t.TempDir(), newMemLabelStore())
	if err != nil {
		t.Fatalf("NewLabeledStore: %v", err)
	}
	return cs
}

// writeTarStream writes a tar archive to w from the given entries.
func writeTarStream(w io.Writer, entries []struct{ name, body string }) error {
	tw := tar.NewWriter(w)
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
			return err
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				return err
			}
		}
	}
	return tw.Close()
}

// makePlainLayer builds a plain (uncompressed) tar blob for use in tests.  For uncompressed tars, diffID == the blob digest -- no gsip needed.
func makePlainLayer(t *testing.T, entries []struct{ name, body string }) ([]byte, digest.Digest) {
	t.Helper()
	var buf bytes.Buffer
	if err := writeTarStream(&buf, entries); err != nil {
		t.Fatalf("writeTarStream: %v", err)
	}
	data := buf.Bytes()
	return data, digest.Canonical.FromBytes(data)
}

// ingestBlob writes data to a content.Store under the given digest.
func ingestBlob(t *testing.T, cs content.Store, data []byte, d digest.Digest, mediaType string) {
	t.Helper()
	ctx := context.Background()
	desc := ocispec.Descriptor{Digest: d, Size: int64(len(data)), MediaType: mediaType}
	w, err := cs.Writer(ctx, content.WithRef(d.String()), content.WithDescriptor(desc))
	if err != nil {
		t.Fatalf("Writer(%s): %v", d, err)
	}
	defer w.Close()
	if _, err := w.Write(data); err != nil {
		t.Fatalf("Write(%s): %v", d, err)
	}
	if err := w.Commit(ctx, int64(len(data)), d); err != nil {
		t.Fatalf("Commit(%s): %v", d, err)
	}
}

// labelBlob sets labels on a blob in the content store.
func labelBlob(t *testing.T, cs content.Store, d digest.Digest, labels map[string]string) {
	t.Helper()
	ctx := context.Background()
	info, err := cs.Info(ctx, d)
	if err != nil {
		t.Fatalf("Info(%s): %v", d, err)
	}
	if info.Labels == nil {
		info.Labels = map[string]string{}
	}
	var fields []string
	for k, v := range labels {
		info.Labels[k] = v
		fields = append(fields, "labels."+k)
	}
	if _, err := cs.Update(ctx, info, fields...); err != nil {
		t.Fatalf("Update(%s): %v", d, err)
	}
}

// TestFindCompressedBlob verifies that findCompressedBlob can locate a layer blob (here, plain uncompressed so diffID == blob digest) by walking the image manifest graph.
func TestFindCompressedBlob(t *testing.T) {
	cs := newTestStore(t)
	ctx := context.Background()

	// plain (uncompressed) layer: diffID == blob digest
	layerData, diffID := makePlainLayer(t, []struct{ name, body string }{
		{"etc/", ""},
		{"etc/hostname", "testhost"},
	})
	compDigest := diffID // uncompressed: same digest
	ingestBlob(t, cs, layerData, compDigest, ocispec.MediaTypeImageLayer)

	// build minimal config
	configJSON := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["` + diffID.String() + `"]}}`)
	configDigest := digest.Canonical.FromBytes(configJSON)
	ingestBlob(t, cs, configJSON, configDigest, ocispec.MediaTypeImageConfig)

	// build manifest that links config and layer
	manifestJSON := []byte(`{"schemaVersion":2,"config":{"digest":"` + configDigest.String() +
		`","size":` + strconv.Itoa(len(configJSON)) + `},"layers":[{"digest":"` +
		compDigest.String() + `","size":` + strconv.Itoa(len(layerData)) + `}]}`)
	manifestDigest := digest.Canonical.FromBytes(manifestJSON)
	ingestBlob(t, cs, manifestJSON, manifestDigest, ocispec.MediaTypeImageManifest)

	// add the gc.ref labels to the manifest (as containerd does during pull)
	labelBlob(t, cs, manifestDigest, map[string]string{
		"containerd.io/gc.ref.content.config": configDigest.String(),
		"containerd.io/gc.ref.content.l.0":    compDigest.String(),
	})

	// test
	found, err := findCompressedBlob(ctx, cs, diffID)
	if err != nil {
		t.Fatalf("findCompressedBlob: %v", err)
	}
	if found != compDigest {
		t.Errorf("findCompressedBlob = %s, want %s", found, compDigest)
	}
}

// TestOpenLayerByDiffID_plain verifies that opening an uncompressed layer blob via diffID produces a working fs.FS backed by tarfs (fast path: diffID == blob digest, no gsip needed).
func TestOpenLayerByDiffID_plain(t *testing.T) {
	cs := newTestStore(t)
	ctx := context.Background()

	entries := []struct{ name, body string }{
		{"bin/", ""},
		{"bin/hello", "#!/bin/sh\necho hi\n"},
		{"etc/", ""},
		{"etc/greeting", "hello from tarfs"},
	}
	layerData, diffID := makePlainLayer(t, entries)
	// for uncompressed blobs, the blob digest IS the diffID
	ingestBlob(t, cs, layerData, diffID, ocispec.MediaTypeImageLayer)

	sn := &Snapshotter{cs: cs}
	layerFS, err := sn.openLayerByDiffID(ctx, diffID)
	if err != nil {
		t.Fatalf("openLayerByDiffID: %v", err)
	}

	got, err := fs.ReadFile(layerFS, "etc/greeting")
	if err != nil {
		t.Fatalf("ReadFile etc/greeting: %v", err)
	}
	if string(got) != "hello from tarfs" {
		t.Errorf("greeting = %q, want %q", string(got), "hello from tarfs")
	}

	// verify directory listing
	dirEntries, err := fs.ReadDir(layerFS, "bin")
	if err != nil {
		t.Fatalf("ReadDir bin: %v", err)
	}
	if len(dirEntries) != 1 || dirEntries[0].Name() != "hello" {
		t.Errorf("bin entries = %v, want [hello]", dirEntries)
	}
}

// TestOpenLayerByDiffID_multiLayer tests opening layers in a stacked image (LayeredFS with whiteouts).
func TestOpenLayerByDiffID_multiLayer(t *testing.T) {
	cs := newTestStore(t)
	ctx := context.Background()

	// plain (uncompressed) layers: diffID == blob digest, no gsip needed
	base := []struct{ name, body string }{
		{"etc/", ""},
		{"etc/hostname", "basehost"},
		{"etc/secret", "s3cr3t"},
	}
	baseData, baseDiff := makePlainLayer(t, base)
	ingestBlob(t, cs, baseData, baseDiff, ocispec.MediaTypeImageLayer)

	top := []struct{ name, body string }{
		{"etc/", ""},
		{"etc/hostname", "tophost"},
		{"etc/.wh.secret", ""},
	}
	topData, topDiff := makePlainLayer(t, top)
	ingestBlob(t, cs, topData, topDiff, ocispec.MediaTypeImageLayer)

	// set up manifests with gc.ref labels so findCompressedBlob can find them
	for _, pair := range []struct {
		blobDigest digest.Digest
		data       []byte
	}{{baseDiff, baseData}, {topDiff, topData}} {
		configJSON := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["` + pair.blobDigest.String() + `"]}}`)
		configDigest := digest.Canonical.FromBytes(configJSON)
		ingestBlob(t, cs, configJSON, configDigest, ocispec.MediaTypeImageConfig)

		manifestJSON := []byte(`{"schemaVersion":2,"config":{"digest":"` + configDigest.String() + `","size":` + strconv.Itoa(len(configJSON)) + `},"layers":[{"digest":"` + pair.blobDigest.String() + `","size":` + strconv.Itoa(len(pair.data)) + `}]}`)
		manifestDigest := digest.Canonical.FromBytes(manifestJSON)
		ingestBlob(t, cs, manifestJSON, manifestDigest, ocispec.MediaTypeImageManifest)
		labelBlob(t, cs, manifestDigest, map[string]string{
			"containerd.io/gc.ref.content.config": configDigest.String(),
			"containerd.io/gc.ref.content.l.0":    pair.blobDigest.String(),
		})
	}

	sn := &Snapshotter{cs: cs}

	baseFS, err := sn.openLayerByDiffID(ctx, baseDiff)
	if err != nil {
		t.Fatalf("openLayerByDiffID base: %v", err)
	}
	topFS, err := sn.openLayerByDiffID(ctx, topDiff)
	if err != nil {
		t.Fatalf("openLayerByDiffID top: %v", err)
	}

	// top-first order for LayeredFS
	merged := &LayeredFS{layers: []fs.FS{topFS, baseFS}}

	// hostname should be from top layer
	got, err := fs.ReadFile(merged, "etc/hostname")
	if err != nil {
		t.Fatalf("ReadFile hostname: %v", err)
	}
	if string(got) != "tophost" {
		t.Errorf("hostname = %q, want %q", string(got), "tophost")
	}

	// secret should be whited out
	_, err = merged.Open("etc/secret")
	if err == nil {
		t.Error("Open etc/secret: expected error (whiteout), got nil")
	}
}
