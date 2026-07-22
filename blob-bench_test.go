package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/jonjohnsonjr/targz/tarfs"
)

// makeGzipBlobB builds a gzip-compressed tar whose uncompressed size is approximately targetBytes.
// The content is moderately compressible (10-char repeating pattern).
func makeGzipBlobB(b *testing.B, targetBytes int) (compData, plainData []byte) {
	b.Helper()
	var plainBuf bytes.Buffer
	tw := tar.NewWriter(&plainBuf)
	body := strings.Repeat("abcdefghij", 100) // 1 KiB per entry, compresses ~5x
	for plainBuf.Len() < targetBytes {
		hdr := &tar.Header{Name: fmt.Sprintf("f%d", plainBuf.Len()), Mode: 0o644, Size: int64(len(body))}
		if err := tw.WriteHeader(hdr); err != nil {
			b.Fatalf("WriteHeader: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			b.Fatalf("Write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		b.Fatalf("tar close: %v", err)
	}
	plainData = plainBuf.Bytes()
	var compBuf bytes.Buffer
	gw := gzip.NewWriter(&compBuf)
	if _, err := gw.Write(plainData); err != nil {
		b.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		b.Fatalf("gzip close: %v", err)
	}
	return compBuf.Bytes(), plainData
}

// BenchmarkOpenBlob_gzip compares two steady-state paths for serving a gzip layer:
//
//   - decompress: gzip → bytes.Buffer → tarfs.New on every call (current path for blobs < 4 MiB)
//   - fastPath: content store lookup → tarfs.New after the first-call ingest
//
// The 6 MiB case sits just above the gsip threshold and is included for reference.
func BenchmarkOpenBlob_gzip(b *testing.B) {
	sizes := []struct {
		name  string
		bytes int
	}{
		{"4KB", 4 << 10},
		{"64KB", 64 << 10},
		{"512KB", 512 << 10},
		{"2MB", 2 << 20},
		{"6MB", 6 << 20},
	}

	ctx := context.Background()

	for _, sz := range sizes {
		compData, plainData := makeGzipBlobB(b, sz.bytes)
		compDigest := digest.Canonical.FromBytes(compData)
		diffID := digest.Canonical.FromBytes(plainData)

		b.Run("decompress/"+sz.name, func(b *testing.B) {
			b.SetBytes(int64(len(plainData)))
			b.ReportAllocs()
			for b.Loop() {
				gr, err := gzip.NewReader(bytes.NewReader(compData))
				if err != nil {
					b.Fatal(err)
				}
				var buf bytes.Buffer
				if _, err := io.Copy(&buf, gr); err != nil {
					b.Fatal(err)
				}
				gr.Close()
				data := buf.Bytes()
				if _, err := tarfs.New(bytes.NewReader(data), int64(len(data))); err != nil {
					b.Fatal(err)
				}
			}
		})

		b.Run("fastPath/"+sz.name, func(b *testing.B) {
			cs := newTestStore(b)
			ingestBlob(b, cs, compData, compDigest, ocispec.MediaTypeImageLayerGzip)
			labelContentBlob(b, cs, compDigest, map[string]string{"containerd.io/uncompressed": diffID.String()})
			sn := &Snapshotter{cs: cs}
			// first call: populates the cache
			gr, _ := gzip.NewReader(bytes.NewReader(compData))
			if _, _, err := sn.ingestDecompressedBlob(ctx, compDigest, gr); err != nil {
				b.Fatalf("pre-populate: %v", err)
			}
			gr.Close()

			b.SetBytes(int64(len(plainData)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				// gc.ref label is set; ingestDecompressedBlob returns from the fast path without reading the reader
				ra, size, err := sn.ingestDecompressedBlob(ctx, compDigest, bytes.NewReader(nil))
				if err != nil {
					b.Fatal(err)
				}
				if _, err := tarfs.New(ra, size); err != nil {
					b.Fatal(err)
				}
				ra.Close()
			}
		})
	}
}
