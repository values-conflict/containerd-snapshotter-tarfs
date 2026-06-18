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

	"github.com/klauspost/compress/zstd"
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

// labelContentBlob sets labels on a blob in the content store.
func labelContentBlob(t *testing.T, cs content.Store, d digest.Digest, labels map[string]string) {
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

// makeZstdLayer writes entries into a zstd-compressed tar and returns the compressed bytes and the uncompressed tar bytes
func makeZstdLayer(t *testing.T, entries []struct{ name, body string }) ([]byte, []byte) {
	t.Helper()
	var plainBuf bytes.Buffer
	if err := writeTarStream(&plainBuf, entries); err != nil {
		t.Fatalf("writeTarStream: %v", err)
	}
	plainData := plainBuf.Bytes()
	var compBuf bytes.Buffer
	zw, err := zstd.NewWriter(&compBuf)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	if _, err := zw.Write(plainData); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	return compBuf.Bytes(), plainData
}

// noReadReader is a reader that fails the test if Read is ever called; used to assert fast paths skip decompression
type noReadReader struct{ t *testing.T }

func (r *noReadReader) Read(_ []byte) (int, error) {
	r.t.Fatal("Read called: fast path should have returned before reaching decompression")
	return 0, nil
}

// TestIngestDecompressedBlob_sha256DiffID verifies the normal path: known sha256 diffID, blob stored at that address, labels set correctly
func TestIngestDecompressedBlob_sha256DiffID(t *testing.T) {
	cs := newTestStore(t)
	ctx := context.Background()

	compData, plainData := makeZstdLayer(t, []struct{ name, body string }{{"hello", "world\n"}})
	compDigest := digest.Canonical.FromBytes(compData)
	diffID := digest.Canonical.FromBytes(plainData)

	ingestBlob(t, cs, compData, compDigest, "application/vnd.oci.image.layer.v1.tar+zstd")
	labelContentBlob(t, cs, compDigest, map[string]string{"containerd.io/uncompressed": diffID.String()})

	sn := &Snapshotter{cs: cs}
	zr, err := zstd.NewReader(bytes.NewReader(compData))
	if err != nil {
		t.Fatalf("zstd.NewReader: %v", err)
	}
	defer zr.Close()

	ra, size, err := sn.ingestDecompressedBlob(ctx, compDigest, zr)
	if err != nil {
		t.Fatalf("ingestDecompressedBlob: %v", err)
	}
	defer ra.Close()

	got := make([]byte, size)
	if _, err := ra.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, plainData) {
		t.Errorf("decompressed content mismatch: got %d bytes, want %d", len(got), len(plainData))
	}

	info, err := cs.Info(ctx, compDigest)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if got := info.Labels["containerd.io/gc.ref.content.uncompressed"]; got != diffID.String() {
		t.Errorf("gc.ref label = %q, want %q", got, diffID)
	}
	// containerd.io/uncompressed must not be overwritten
	if got := info.Labels["containerd.io/uncompressed"]; got != diffID.String() {
		t.Errorf("uncompressed label = %q, want %q", got, diffID)
	}
}

// TestIngestDecompressedBlob_noDiffIDLabel verifies that ingestDecompressedBlob discovers the diffID via cw.Digest() and sets containerd.io/uncompressed when no label exists upfront
func TestIngestDecompressedBlob_noDiffIDLabel(t *testing.T) {
	cs := newTestStore(t)
	ctx := context.Background()

	compData, plainData := makeZstdLayer(t, []struct{ name, body string }{{"file", "content\n"}})
	compDigest := digest.Canonical.FromBytes(compData)
	wantDiffID := digest.Canonical.FromBytes(plainData)

	// store compressed blob without any diffID label
	ingestBlob(t, cs, compData, compDigest, "application/vnd.oci.image.layer.v1.tar+zstd")

	sn := &Snapshotter{cs: cs}
	zr, err := zstd.NewReader(bytes.NewReader(compData))
	if err != nil {
		t.Fatalf("zstd.NewReader: %v", err)
	}
	defer zr.Close()

	if _, _, err := sn.ingestDecompressedBlob(ctx, compDigest, zr); err != nil {
		t.Fatalf("ingestDecompressedBlob: %v", err)
	}

	info, err := cs.Info(ctx, compDigest)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if got := info.Labels["containerd.io/uncompressed"]; got != wantDiffID.String() {
		t.Errorf("uncompressed label = %q, want %q", got, wantDiffID)
	}
	if got := info.Labels["containerd.io/gc.ref.content.uncompressed"]; got != wantDiffID.String() {
		t.Errorf("gc.ref label = %q, want %q", got, wantDiffID)
	}
}

// TestIngestDecompressedBlob_fastPath verifies that a second call returns the cached blob without invoking the decompressor
func TestIngestDecompressedBlob_fastPath(t *testing.T) {
	cs := newTestStore(t)
	ctx := context.Background()

	compData, plainData := makeZstdLayer(t, []struct{ name, body string }{{"f", "v\n"}})
	compDigest := digest.Canonical.FromBytes(compData)
	diffID := digest.Canonical.FromBytes(plainData)

	ingestBlob(t, cs, compData, compDigest, "application/vnd.oci.image.layer.v1.tar+zstd")
	labelContentBlob(t, cs, compDigest, map[string]string{"containerd.io/uncompressed": diffID.String()})

	sn := &Snapshotter{cs: cs}

	// first call: populates cache and sets gc.ref label
	zr, _ := zstd.NewReader(bytes.NewReader(compData))
	ra1, size1, err := sn.ingestDecompressedBlob(ctx, compDigest, zr)
	zr.Close()
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	ra1.Close()

	// second call: must hit fast path via gc.ref label and never call Read on the decompressor
	ra2, size2, err := sn.ingestDecompressedBlob(ctx, compDigest, &noReadReader{t})
	if err != nil {
		t.Fatalf("second call (fast path): %v", err)
	}
	defer ra2.Close()
	if size1 != size2 {
		t.Errorf("size mismatch: first=%d second=%d", size1, size2)
	}
}

// TestIngestDecompressedBlob_sha512DiffID verifies that when containerd.io/uncompressed carries a sha512 diffID the writer is initialized with sha512 and the blob is stored at the sha512 address
func TestIngestDecompressedBlob_sha512DiffID(t *testing.T) {
	cs := newTestStore(t)
	ctx := context.Background()

	compData, plainData := makeZstdLayer(t, []struct{ name, body string }{{"hello", "world\n"}})
	compDigest := digest.Canonical.FromBytes(compData)
	sha512DiffID := digest.SHA512.FromBytes(plainData)

	ingestBlob(t, cs, compData, compDigest, "application/vnd.oci.image.layer.v1.tar+zstd")
	labelContentBlob(t, cs, compDigest, map[string]string{"containerd.io/uncompressed": sha512DiffID.String()})

	sn := &Snapshotter{cs: cs}
	zr, err := zstd.NewReader(bytes.NewReader(compData))
	if err != nil {
		t.Fatalf("zstd.NewReader: %v", err)
	}
	defer zr.Close()

	ra, size, err := sn.ingestDecompressedBlob(ctx, compDigest, zr)
	if err != nil {
		t.Fatalf("ingestDecompressedBlob: %v", err)
	}
	defer ra.Close()

	got := make([]byte, size)
	if _, err := ra.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, plainData) {
		t.Errorf("decompressed content mismatch: got %d bytes, want %d", len(got), len(plainData))
	}

	// blob must be at the sha512 address
	ucInfo, err := cs.Info(ctx, sha512DiffID)
	if err != nil {
		t.Fatalf("decompressed blob not found at sha512 diffID %s: %v", sha512DiffID, err)
	}
	if ucInfo.Size != int64(len(plainData)) {
		t.Errorf("blob size at sha512 address = %d, want %d", ucInfo.Size, len(plainData))
	}

	info, err := cs.Info(ctx, compDigest)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if got := info.Labels["containerd.io/gc.ref.content.uncompressed"]; got != sha512DiffID.String() {
		t.Errorf("gc.ref label = %q, want %q", got, sha512DiffID)
	}
	// containerd.io/uncompressed must remain sha512, not be overwritten with sha256
	if got := info.Labels["containerd.io/uncompressed"]; got != sha512DiffID.String() {
		t.Errorf("uncompressed label = %q, want %q", got, sha512DiffID)
	}

	// second call: fast path via gc.ref (sha512 address), no decompression
	ra2, size2, err := sn.ingestDecompressedBlob(ctx, compDigest, &noReadReader{t})
	if err != nil {
		t.Fatalf("second call (fast path): %v", err)
	}
	defer ra2.Close()
	if size2 != int64(len(plainData)) {
		t.Errorf("fast path size = %d, want %d", size2, len(plainData))
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
		labelContentBlob(t, cs, manifestDigest, map[string]string{
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
