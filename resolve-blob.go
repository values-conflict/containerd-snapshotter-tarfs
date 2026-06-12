package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/containerd/containerd/v2/core/content"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ociConfig is a minimal subset of the OCI image config we need.
type ociConfig struct {
	RootFS struct {
		DiffIDs []digest.Digest `json:"diff_ids"`
	} `json:"rootfs"`
}

// findCompressedBlob locates the compressed layer blob for diffID by walking the image manifests in the content store.
//
// Platform manifests carry containerd.io/gc.ref.content.l.N labels that point directly to their layer blobs, and containerd.io/gc.ref.content.config pointing to the config blob.  We match diffID against the config's rootfs.diff_ids array, then return the blob at the same index.
func findCompressedBlob(ctx context.Context, cs content.Store, diffID digest.Digest) (digest.Digest, error) {
	var result digest.Digest

	walkErr := cs.Walk(ctx, func(info content.Info) error {
		// skip blobs without layer references -- only platform manifests have these
		configDigestStr, hasConfig := info.Labels["containerd.io/gc.ref.content.config"]
		if !hasConfig {
			return nil
		}
		configDigest, err := digest.Parse(configDigestStr)
		if err != nil {
			return nil
		}

		// collect the layer digests from the manifest labels
		var layerDigests []digest.Digest
		for i := 0; ; i++ {
			key := fmt.Sprintf("containerd.io/gc.ref.content.l.%d", i)
			val, ok := info.Labels[key]
			if !ok {
				break
			}
			d, err := digest.Parse(val)
			if err != nil {
				break
			}
			layerDigests = append(layerDigests, d)
		}
		if len(layerDigests) == 0 {
			return nil
		}

		// read and parse the config blob
		ra, err := cs.ReaderAt(ctx, ocispec.Descriptor{Digest: configDigest})
		if err != nil {
			return nil
		}
		defer ra.Close()

		buf := make([]byte, ra.Size())
		if _, err := ra.ReadAt(buf, 0); err != nil {
			return nil
		}

		var cfg ociConfig
		if err := json.Unmarshal(buf, &cfg); err != nil {
			return nil
		}

		// find diffID in the config's diff_ids list
		for i, d := range cfg.RootFS.DiffIDs {
			if d == diffID && i < len(layerDigests) {
				result = layerDigests[i]
				// returning an error stops the Walk early
				return errFound
			}
		}
		return nil
	})

	if walkErr != nil && walkErr != errFound {
		return "", fmt.Errorf("walking content store for compressed blob: %w", walkErr)
	}
	if result == "" {
		return "", fmt.Errorf("no manifest found linking to diffID %s", diffID)
	}
	return result, nil
}

// errFound is a sentinel used to stop a content.Walk early once we find a match.
var errFound = fmt.Errorf("found")

// labelLayerBlob adds containerd.io/uncompressed=diffID to the compressed blob in the content store.  This enables the standard Walk-based lookup in openLayerByDiffID on future calls.  Errors are non-fatal -- the label is an optimisation.
func labelLayerBlob(ctx context.Context, cs content.Store, compDigest, diffID digest.Digest) {
	info, err := cs.Info(ctx, compDigest)
	if err != nil {
		return
	}
	if info.Labels == nil {
		info.Labels = map[string]string{}
	}
	if info.Labels["containerd.io/uncompressed"] == diffID.String() {
		return // already labeled
	}
	info.Labels["containerd.io/uncompressed"] = diffID.String()
	_, _ = cs.Update(ctx, info, "labels.containerd.io/uncompressed")
}
