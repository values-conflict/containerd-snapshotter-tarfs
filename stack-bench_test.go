package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// stackFixture holds a pre-populated content store and the topChainID for BenchmarkBuildLayerStack.
type stackFixture struct {
	sn         *Snapshotter
	topChainID string
}

// buildStackFixture creates a content store populated with a single N-layer OCI image: N minimal
// plain-tar layer blobs, one config, and one manifest.  If labeled is true, the manifest carries
// the containerd.io/gc.ref.content.config label so buildLayerStack finds it via the PASS 1 fast
// path (labeled-manifest walk); otherwise only PASS 2 (full blob scan) applies.
func buildStackFixture(b *testing.B, depth int, labeled bool) stackFixture {
	b.Helper()
	cs := newTestStore(b)

	diffIDs := make([]digest.Digest, depth)
	layerDigests := make([]digest.Digest, depth)
	layerSizes := make([]int, depth)

	for i := range depth {
		data, d := makePlainLayer(b, []struct{ name, body string }{
			{fmt.Sprintf("l%d/", i), ""},
			{fmt.Sprintf("l%d/f", i), fmt.Sprintf("%d\n", i)},
		})
		ingestBlob(b, cs, data, d, ocispec.MediaTypeImageLayer)
		diffIDs[i] = d
		layerDigests[i] = d
		layerSizes[i] = len(data)
	}

	// compute the OCI chainID for the full stack
	chain := diffIDs[0]
	for _, d := range diffIDs[1:] {
		chain = digest.Canonical.FromString(chain.String() + " " + d.String())
	}

	// config JSON: rootfs.diff_ids lists all layers
	diffIDStrs := make([]string, depth)
	for i, d := range diffIDs {
		diffIDStrs[i] = d.String()
	}
	configData, err := json.Marshal(struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		RootFS       struct {
			Type    string   `json:"type"`
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}{
		Architecture: "amd64",
		OS:           "linux",
		RootFS: struct {
			Type    string   `json:"type"`
			DiffIDs []string `json:"diff_ids"`
		}{"layers", diffIDStrs},
	})
	if err != nil {
		b.Fatalf("marshal config: %v", err)
	}
	configDigest := digest.Canonical.FromBytes(configData)
	ingestBlob(b, cs, configData, configDigest, ocispec.MediaTypeImageConfig)

	// manifest JSON: config.digest + all layer digests in order
	type descJSON struct {
		Digest string `json:"digest"`
		Size   int    `json:"size"`
	}
	layerDescs := make([]descJSON, depth)
	for i := range depth {
		layerDescs[i] = descJSON{layerDigests[i].String(), layerSizes[i]}
	}
	manifestData, err := json.Marshal(struct {
		SchemaVersion int        `json:"schemaVersion"`
		Config        descJSON   `json:"config"`
		Layers        []descJSON `json:"layers"`
	}{2, descJSON{configDigest.String(), len(configData)}, layerDescs})
	if err != nil {
		b.Fatalf("marshal manifest: %v", err)
	}
	manifestDigest := digest.Canonical.FromBytes(manifestData)
	ingestBlob(b, cs, manifestData, manifestDigest, ocispec.MediaTypeImageManifest)

	if labeled {
		labels := map[string]string{
			"containerd.io/gc.ref.content.config": configDigest.String(),
		}
		for i, d := range layerDigests {
			labels[fmt.Sprintf("containerd.io/gc.ref.content.l.%d", i)] = d.String()
		}
		labelContentBlob(b, cs, manifestDigest, labels)
	}

	return stackFixture{sn: &Snapshotter{cs: cs}, topChainID: chain.String()}
}

// BenchmarkBuildLayerStack measures the full buildLayerStack call: OCI manifest walk, config
// JSON parse, chainID recomputation, and opening each layer blob via tarfs.  This is the cost
// tarfs pays on every View/Prepare that the overlay snapshotter does not.
//
// Sub-benchmarks vary stack depth and manifest discoverability:
//   - labeled: manifest carries containerd.io/gc.ref.content.config; PASS 1 finds it directly
//   - unlabeled: no label; PASS 2 scans every blob in the store
func BenchmarkBuildLayerStack(b *testing.B) {
	for _, depth := range []int{1, 5, 10, 20} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			b.Run("labeled", func(b *testing.B) {
				fix := buildStackFixture(b, depth, true)
				ctx := context.Background()
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					layers, err := fix.sn.buildLayerStack(ctx, fix.topChainID)
					if err != nil {
						b.Fatal(err)
					}
					_ = layers
				}
			})

			b.Run("unlabeled", func(b *testing.B) {
				fix := buildStackFixture(b, depth, false)
				ctx := context.Background()
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					layers, err := fix.sn.buildLayerStack(ctx, fix.topChainID)
					if err != nil {
						b.Fatal(err)
					}
					_ = layers
				}
			})
		})
	}
}
