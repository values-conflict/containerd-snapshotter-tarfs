package main

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"

	diffapi "github.com/containerd/containerd/api/services/diff/v1"
	containerdtypes "github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/klauspost/compress/zstd"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TarfsDiffer implements the diff.v1.Diff gRPC service for the tardiffs proxy plugin, computing the diffID (sha256 of the uncompressed layer tar) by streaming and hashing the blob in memory without writing any files to disk -- unlike the default walking differ, which extracts each layer to a temporary directory.
type TarfsDiffer struct {
	diffapi.UnimplementedDiffServer
	cs content.Store
}

// NewTarfsDiffer creates a TarfsDiffer backed by cs for blob access.
func NewTarfsDiffer(cs content.Store) *TarfsDiffer {
	return &TarfsDiffer{cs: cs}
}

// Apply streams and decompresses the blob described by req, hashing the uncompressed bytes to produce the diffID without writing any files to disk.  Non-empty mounts indicate a non-tarfs snapshotter (overlayfs, native, etc.); in that case Apply returns codes.Unimplemented so the diff service falls through to the walking differ, which handles the extraction correctly.  The tarfs snapshotter always returns nil mounts for extraction-keyed snapshots, so zero-mount calls are always tarfs extractions.
func (d *TarfsDiffer) Apply(ctx context.Context, req *diffapi.ApplyRequest) (*diffapi.ApplyResponse, error) {
	// non-empty mounts = non-tarfs snapshotter; signal the diff service to fall through to walking
	if len(req.Mounts) > 0 {
		return nil, status.Errorf(codes.Unimplemented, "non-empty mounts: not a tarfs extraction snapshot")
	}

	if req.Diff == nil {
		return nil, fmt.Errorf("apply request missing diff descriptor")
	}

	blobDigest, err := digest.Parse(req.Diff.Digest)
	if err != nil {
		return nil, fmt.Errorf("parsing blob digest %q: %w", req.Diff.Digest, err)
	}

	pctx := propagateNamespace(ctx)
	ra, err := d.cs.ReaderAt(pctx, ocispec.Descriptor{Digest: blobDigest, Size: req.Diff.Size})
	if err != nil {
		return nil, fmt.Errorf("opening blob %s: %w", blobDigest, err)
	}
	defer ra.Close()

	size := ra.Size()

	// detect compression by magic bytes (same scheme as openBlobAsFS)
	header := make([]byte, 4)
	if _, err := ra.ReadAt(header, 0); err != nil {
		return nil, fmt.Errorf("reading header of %s: %w", blobDigest, err)
	}

	var r io.Reader
	switch {
	case header[0] == 0x1f && header[1] == 0x8b:
		// gzip-compressed blob
		gz, err := gzip.NewReader(io.NewSectionReader(ra, 0, size))
		if err != nil {
			return nil, fmt.Errorf("creating gzip reader for %s: %w", blobDigest, err)
		}
		defer gz.Close()
		r = gz
	case header[0] == 0x28 && header[1] == 0xB5 && header[2] == 0x2F && header[3] == 0xFD:
		// zstd-compressed blob
		zr, err := zstd.NewReader(io.NewSectionReader(ra, 0, size))
		if err != nil {
			return nil, fmt.Errorf("creating zstd reader for %s: %w", blobDigest, err)
		}
		defer zr.Close()
		r = zr
	default:
		// uncompressed tar -- hash directly
		r = io.NewSectionReader(ra, 0, size)
	}

	digester := digest.Canonical.Digester()
	n, err := io.Copy(digester.Hash(), r)
	if err != nil {
		return nil, fmt.Errorf("hashing blob %s: %w", blobDigest, err)
	}

	return &diffapi.ApplyResponse{
		Applied: &containerdtypes.Descriptor{
			MediaType: ocispec.MediaTypeImageLayer,
			Digest:    digester.Digest().String(),
			Size:      n,
		},
	}, nil
}
