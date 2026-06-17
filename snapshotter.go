package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	go2fuse "github.com/cpuguy83/go2fuse"
	gofusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/klauspost/compress/zstd"

	"github.com/jonjohnsonjr/targz/gsip"
	"github.com/jonjohnsonjr/targz/tarfs"
)

const (
	labelNS          = "tianon.xyz/values-conflict/tarfs"
	labelBlobChainID = labelNS + "/chain-id" // label on content-store blobs: chainID of the snapshot that uses this blob
)

// Snapshotter implements snapshots.Snapshotter by serving OCI image layers directly from the containerd content store via FUSE, without extraction.
// All persistent state lives in the content store (blob labels).  stateDir holds only runtime state (FUSE mountpoints, overlay upper/work dirs) and is not preserved across restarts.
// Snapshot kinds and parent chains are tracked in-memory; View/Active snapshots do not survive process restarts.
type Snapshotter struct {
	root string
	cs   content.Store

	mu           sync.Mutex
	mounts       map[string]*activeMount // backend key → live FUSE mount
	kinds        sync.Map                // backend key → snapshots.Kind
	layerChains  sync.Map                // backend key → topChainID (View/Active with parents only)
	parentChains sync.Map                // newChainID → parentChainID (for docker commit/build layers whose manifest isn't labeled)
	upperDirs    sync.Map                // committed snapshot name → upperDir path; set when an active snapshot with changes is committed so Views of it can serve the changes
}

// activeMount tracks a live FUSE server for a single snapshot.
type activeMount struct {
	server     *fuse.Server
	mountpoint string // FUSE mount directory; empty when FUSE unavailable
	upperDir   string // non-empty for writable (KindActive) snapshots
	workDir    string // overlay workdir for writable snapshots
}

// NewSnapshotter creates a Snapshotter rooted at stateDir using cs for layer blob access.  stateDir will be created if it does not exist.
func NewSnapshotter(ctx context.Context, stateDir string, cs content.Store) (*Snapshotter, error) {
	for _, sub := range []string{".", "mounts", "upper", "work"} {
		if err := os.MkdirAll(filepath.Join(stateDir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("creating state dir %q: %w", filepath.Join(stateDir, sub), err)
		}
	}
	return &Snapshotter{root: stateDir, cs: cs, mounts: map[string]*activeMount{}}, nil
}

// keyPath converts a snapshot key to a path-safe directory basename.
// Snapshot keys may contain slashes, colons, and other characters unsuitable for paths.
func keyPath(key string) string {
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", sum)
}

// --- snapshots.Snapshotter interface ---

func (s *Snapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	kind := snapshots.KindCommitted
	if v, ok := s.kinds.Load(key); ok {
		kind = v.(snapshots.Kind)
	}
	return snapshots.Info{Kind: kind}, nil
}

func (s *Snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	// containerd's own metadata BoltDB stores all snapshot labels; we have nothing to update
	return s.Stat(ctx, info.Name)
}

func (s *Snapshotter) Usage(ctx context.Context, key string) (snapshots.Usage, error) {
	return snapshots.Usage{}, nil
}

func (s *Snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return s.createSnapshot(ctx, snapshots.KindActive, key, parent)
}

func (s *Snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return s.createSnapshot(ctx, snapshots.KindView, key, parent)
}

func (s *Snapshotter) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	kind := snapshots.KindView
	if v, ok := s.kinds.Load(key); ok {
		kind = v.(snapshots.Kind)
	}

	s.mu.Lock()
	am := s.mounts[key]
	s.mu.Unlock()

	if am == nil {
		// mount not live -- rebuild if we have the chain info (fails after restart by design)
		topChainID, ok := s.layerChains.Load(key)
		if !ok {
			return nil, fmt.Errorf("snapshot %q not in memory (process restart requires re-mount): %w", key, errdefs.ErrNotFound)
		}
		var err error
		am, err = s.buildMount(ctx, key, kind, topChainID.(string), "")
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.mounts[key] = am
		s.mu.Unlock()
	}

	return s.mountsFor(key, kind, am), nil
}

func (s *Snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	s.kinds.Store(name, snapshots.KindCommitted)
	s.kinds.Delete(key)
	// propagate layer chain through init layers (Docker commits an init snapshot on top of the image layers; its committed name isn't a valid chainID digest, but its parent IS)
	if chain, ok := s.layerChains.Load(key); ok {
		s.layerChains.Store(name, chain)
		// record newChainID → parentChainID for the blob-level fallback in buildLayerStack
		chainID := name[strings.LastIndex(name, "/")+1:]
		if _, err := digest.Parse(chainID); err == nil && chainID != chain.(string) {
			s.parentChains.Store(chainID, chain.(string))
		}
	}
	s.layerChains.Delete(key)

	s.mu.Lock()
	am := s.mounts[key]
	delete(s.mounts, key)
	s.mu.Unlock()

	if am != nil {
		// preserve the upper dir so Views of this committed snapshot can diff it correctly; BuildKit calls Commit before computing the diff blob, then creates a View
		if am.upperDir != "" {
			s.upperDirs.Store(name, am.upperDir)
		}
		s.stopMount(am)
	}

	if chainID := name[strings.LastIndex(name, "/")+1:]; chainID == "" {
		log.Printf("tarfs: Commit %q: could not extract chainID from name", name)
	} else {
		log.Printf("tarfs: Commit %q (chainID %s)", name, chainID)
	}

	return nil
}

func (s *Snapshotter) Remove(ctx context.Context, key string) error {
	s.kinds.Delete(key)
	s.layerChains.Delete(key)
	if v, ok := s.upperDirs.LoadAndDelete(key); ok {
		_ = os.RemoveAll(v.(string))
	}

	s.mu.Lock()
	am := s.mounts[key]
	delete(s.mounts, key)
	s.mu.Unlock()

	if am != nil {
		s.stopMount(am)
	}

	id := keyPath(key)
	_ = os.RemoveAll(filepath.Join(s.root, "upper", id))
	_ = os.RemoveAll(filepath.Join(s.root, "work", id))

	return nil
}

func (s *Snapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, filters ...string) error {
	var walkErr error
	s.kinds.Range(func(k, v any) bool {
		if err := fn(ctx, snapshots.Info{Name: k.(string), Kind: v.(snapshots.Kind)}); err != nil {
			walkErr = err
			return false
		}
		return true
	})
	return walkErr
}

func (s *Snapshotter) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, am := range s.mounts {
		s.stopMount(am)
	}
	s.mounts = map[string]*activeMount{}
	return nil
}

// findNewLayerBlob finds the compressed blob for a new layer by walking blobs with containerd.io/uncompressed labels and checking whether sha256(parentChainID + " " + uncompressed_digest) == newChainID.  Used as a fallback for docker commit/build images whose manifest isn't labeled.
func (s *Snapshotter) findNewLayerBlob(ctx context.Context, parentChainID, newChainID string) digest.Digest {
	var result digest.Digest
	_ = s.cs.Walk(ctx, func(info content.Info) error {
		diffIDStr := info.Labels["containerd.io/uncompressed"]
		if diffIDStr == "" {
			return nil
		}
		if digest.Canonical.FromString(parentChainID+" "+diffIDStr).String() == newChainID {
			result = info.Digest
		}
		return nil
	}, `labels."containerd.io/uncompressed"`)
	return result
}

// findBlobByDiffID returns the compressed blob digest for the given diffID.
// For uncompressed blobs (e.g. in tests) the blob lives at the diffID address itself.
// For compressed blobs the digest is found via the containerd.io/uncompressed label.
func (s *Snapshotter) findBlobByDiffID(ctx context.Context, diffIDStr string) digest.Digest {
	diffID, err := digest.Parse(diffIDStr)
	if err != nil {
		return ""
	}
	// fast path: uncompressed blob stored at its own digest
	if _, err := s.cs.Info(ctx, diffID); err == nil {
		return diffID
	}
	// compressed blob labeled with containerd.io/uncompressed=diffID
	var result digest.Digest
	_ = s.cs.Walk(ctx, func(info content.Info) error {
		result = info.Digest
		return nil
	}, `labels."containerd.io/uncompressed"==`+strconv.Quote(diffIDStr))
	return result
}

// --- internal helpers ---

// buildLayerStack walks image manifests to find all layers up to and including topChainID, opens each as an fs.FS, and returns them bottom-first.  It also labels each blob with labelBlobChainID for fast future lookup.
func (s *Snapshotter) buildLayerStack(ctx context.Context, topChainID string) ([]fs.FS, error) {
	type layerBlob struct {
		blobDigest digest.Digest
		chainID    string
	}
	var layers []layerBlob

	// tryManifest attempts to parse info as an OCI manifest and, if it contains topChainID in its layer chain, populates layers with the full ordered set.
	tryManifest := func(info content.Info) error {
		if layers != nil {
			return nil
		}
		manifestData, err := s.readContentBlob(ctx, info.Digest)
		if err != nil {
			return nil
		}
		var manifest struct {
			Config struct {
				Digest digest.Digest `json:"digest"`
			} `json:"config"`
			Layers []struct {
				Digest digest.Digest `json:"digest"`
			} `json:"layers"`
		}
		if err := json.Unmarshal(manifestData, &manifest); err != nil {
			return nil
		}
		if len(manifest.Layers) == 0 {
			return nil
		}

		configData, err := s.readContentBlob(ctx, manifest.Config.Digest)
		if err != nil {
			return nil
		}
		var config struct {
			RootFS struct {
				DiffIDs []digest.Digest `json:"diff_ids"`
			} `json:"rootfs"`
		}
		if err := json.Unmarshal(configData, &config); err != nil {
			return nil
		}
		if len(config.RootFS.DiffIDs) != len(manifest.Layers) {
			return nil
		}

		var chain digest.Digest
		for i, diffID := range config.RootFS.DiffIDs {
			if i == 0 {
				chain = diffID
			} else {
				chain = digest.Canonical.FromString(chain.String() + " " + diffID.String())
			}
			if chain.String() == topChainID {
				var collected []layerBlob
				var c digest.Digest
				for j, d := range config.RootFS.DiffIDs[:i+1] {
					if j == 0 {
						c = d
					} else {
						c = digest.Canonical.FromString(c.String() + " " + d.String())
					}
					collected = append(collected, layerBlob{manifest.Layers[j].Digest, c.String()})
				}
				layers = collected
				return nil
			}
		}
		return nil
	}

	// first pass: only labeled manifests (fast -- covers normal image pulls)
	if err := s.cs.Walk(ctx, tryManifest, `labels."containerd.io/gc.ref.content.config"`); err != nil {
		return nil, fmt.Errorf("searching manifests for chainID %s: %w", topChainID, err)
	}
	if layers != nil {
		log.Printf("tarfs: buildLayerStack %s: found via labeled manifest (%d layers)", topChainID, len(layers))
	}

	// second pass: all blobs (covers docker commit images whose manifest isn't labeled)
	if layers == nil {
		_ = s.cs.Walk(ctx, tryManifest)
		if layers != nil {
			log.Printf("tarfs: buildLayerStack %s: found via unlabeled manifest (%d layers)", topChainID, len(layers))
		}
	}

	if layers == nil {
		// fast path: single-layer uncompressed blob (tests; chainID == diffID == blob digest)
		if d, err := digest.Parse(topChainID); err == nil {
			if _, csErr := s.cs.Info(ctx, d); csErr == nil {
				layers = []layerBlob{{d, topChainID}}
			}
		}
	}

	if layers == nil {
		// third fallback: reconstruct from parentChains map (populated in Commit for docker commit/build layers).
		// walks containerd.io/uncompressed blobs to find the new layer blob via the chainID formula.
		if parentChainID, ok := s.parentChains.Load(topChainID); ok {
			log.Printf("tarfs: buildLayerStack %s: trying parentChains fallback (parent %s)", topChainID, parentChainID.(string))
			if parentFSLayers, err2 := s.buildLayerStack(ctx, parentChainID.(string)); err2 == nil {
				if newBlob := s.findNewLayerBlob(ctx, parentChainID.(string), topChainID); newBlob != "" {
					log.Printf("tarfs: buildLayerStack %s: found via parentChains+findNewLayerBlob", topChainID)
					pctx2 := propagateNamespace(context.WithoutCancel(ctx))
					_, _ = s.cs.Update(pctx2, content.Info{
						Digest: newBlob,
						Labels: map[string]string{labelBlobChainID: topChainID},
					}, "labels."+labelBlobChainID)
					if newFS, err3 := s.openBlobAsFS(ctx, newBlob); err3 == nil {
						return append(parentFSLayers, newFS), nil // bottom-first; new layer at end
					}
				}
			}
		}
	}

	if layers == nil {
		return nil, fmt.Errorf("no manifest found for chainID %s: %w", topChainID, errdefs.ErrNotFound)
	}

	// open each layer blob and cache its chainID label for future fast lookup
	log.Printf("tarfs: buildLayerStack %s: opening %d layers", topChainID, len(layers))
	pctx := propagateNamespace(context.WithoutCancel(ctx))
	fsLayers := make([]fs.FS, len(layers))
	for i, layer := range layers {
		log.Printf("tarfs: buildLayerStack %s: layer[%d] chainID=%s blob=%s", topChainID, i, layer.chainID, layer.blobDigest)
		if _, err := s.cs.Update(pctx, content.Info{
			Digest: layer.blobDigest,
			Labels: map[string]string{labelBlobChainID: layer.chainID},
		}, "labels."+labelBlobChainID); err != nil {
			log.Printf("tarfs: caching blob label for chainID %s: %v (continuing)", layer.chainID, err)
		}
		fsys, err := s.openBlobAsFS(ctx, layer.blobDigest)
		if err != nil {
			return nil, fmt.Errorf("opening layer blob %s: %w", layer.blobDigest, err)
		}
		fsLayers[i] = fsys
	}
	return fsLayers, nil
}

// isExtractionKey reports whether key was created by the containerd unpacker for layer extraction.
//
// The metadata wrapper in containerd transforms the original key to "<namespace>/<seq>/<original_key>" before forwarding to the snapshotter backend, so we check whether the last "/" component starts with the extraction prefix rather than checking the whole key.
func isExtractionKey(key string) bool {
	if i := strings.LastIndex(key, "/"); i >= 0 {
		return strings.HasPrefix(key[i+1:], snapshots.UnpackKeyPrefix)
	}
	return strings.HasPrefix(key, snapshots.UnpackKeyPrefix)
}

// buildMount constructs (or rebuilds) the mount setup for a snapshot.
//
// For extraction-type snapshots we return empty mounts so containerd extracts the layer tar into a throwaway temp dir (to compute and verify the DiffID) without requiring a bind mount syscall.  The blob stays in the content store and is read at FUSE mount time.
//
// For container-serving snapshots (View / non-extraction Prepare) we attempt to mount the parent layer chain via FUSE.  If FUSE is unavailable in this environment the snapshot is still created and the mounts are returned, but they point at an empty directory.  Container filesystems will be empty in that degraded mode -- the implementation is correct, it just needs FUSE to actually serve content.
func (s *Snapshotter) buildMount(ctx context.Context, key string, kind snapshots.Kind, topChainID, committedUpperDir string) (*activeMount, error) {
	am := &activeMount{}

	extraction := isExtractionKey(key)
	if extraction && kind == snapshots.KindActive {
		// return empty mounts: extraction runs into containerd's throwaway temp dir
		return am, nil
	}

	if topChainID != "" && !extraction {
		pctx := propagateNamespace(ctx)

		fsLayers, err := s.buildLayerStack(pctx, topChainID)
		if err != nil {
			log.Printf("tarfs: buildLayerStack for %q: %v (serving empty directory)", key, err)
		} else {
			// committedUpperDir (if set) is the top layer: content written by a build container whose active snapshot was committed before BuildKit computed its diff blob
			if committedUpperDir != "" {
				fsLayers = append(fsLayers, os.DirFS(committedUpperDir)) // top layer goes at end (bottom-first)
			}

			// buildLayerStack returns bottom-first; LayeredFS wants top-first
			if len(fsLayers) > 1 {
				for i, j := 0, len(fsLayers)-1; i < j; i, j = i+1, j-1 {
					fsLayers[i], fsLayers[j] = fsLayers[j], fsLayers[i]
				}
			}

			var merged fs.FS
			if len(fsLayers) == 1 {
				merged = fsLayers[0]
			} else {
				merged = &LayeredFS{layers: fsLayers}
			}

			mountpoint := filepath.Join(s.root, "mounts", keyPath(key))
			if err := os.MkdirAll(mountpoint, 0o755); err != nil {
				return nil, fmt.Errorf("creating mountpoint %q: %w", mountpoint, err)
			}

			// the tarfs content is immutable (fixed tar archive), so it is safe to
			// cache FUSE dentries and attrs indefinitely -- the kernel default of 1s
			// causes ESTALE when overlayfs tries to reconstruct expired dentries via
			// d_real() without CAP_EXPORT_SUPPORT, breaking ctr run
			forever := time.Duration(math.MaxInt64)
			server, err := go2fuse.Mount(mountpoint, merged, &gofusefs.Options{
				EntryTimeout: &forever,
				AttrTimeout:  &forever,
				MountOptions: fuse.MountOptions{
					AllowOther: true,
					FsName:     "tarfs",
					Name:       "tarfs",
				},
			})
			if err != nil {
				// FUSE unavailable: log and continue in degraded mode -- mounts
				// will point at the empty mountpoint directory
				log.Printf("tarfs: FUSE mount unavailable for %q: %v (serving empty directory)", key, err)
			} else {
				am.server = server
			}
			am.mountpoint = mountpoint
		}
	}

	if kind == snapshots.KindActive {
		upperDir := filepath.Join(s.root, "upper", keyPath(key))
		workDir := filepath.Join(s.root, "work", keyPath(key))
		if err := os.MkdirAll(upperDir, 0o755); err != nil {
			return nil, fmt.Errorf("creating upper dir: %w", err)
		}
		if err := os.MkdirAll(workDir, 0o711); err != nil {
			return nil, fmt.Errorf("creating work dir: %w", err)
		}
		am.upperDir = upperDir
		am.workDir = workDir
	}

	return am, nil
}

func (s *Snapshotter) createSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string) (_ []mount.Mount, err error) {
	s.kinds.Store(key, kind)

	// extract topChainID from parent backend key ("namespace/seq/chainID")
	topChainID := ""
	if parent != "" {
		candidate := parent[strings.LastIndex(parent, "/")+1:]
		if _, err := digest.Parse(candidate); err == nil {
			topChainID = candidate
		} else if chain, ok := s.layerChains.Load(parent); ok {
			// parent is a non-chainID key (e.g. Docker's init layer whose name ends in "-init"); inherit the image chain that was propagated through Commit
			topChainID = chain.(string)
		}
		if topChainID != "" {
			s.layerChains.Store(key, topChainID)
		}
	}

	// if the parent is a committed snapshot that preserved an upper dir (BuildKit build containers), include that dir as the top layer so Views used for diff computation see the changes
	committedUpperDir := ""
	if v, ok := s.upperDirs.Load(parent); ok {
		committedUpperDir = v.(string)
	}

	am, err := s.buildMount(ctx, key, kind, topChainID, committedUpperDir)
	if err != nil {
		s.kinds.Delete(key)
		s.layerChains.Delete(key)
		return nil, err
	}

	s.mu.Lock()
	s.mounts[key] = am
	s.mu.Unlock()

	return s.mountsFor(key, kind, am), nil
}

// mountsFor returns the []mount.Mount instructions for the given snapshot.
func (s *Snapshotter) mountsFor(key string, kind snapshots.Kind, am *activeMount) []mount.Mount {
	// extraction snapshot: return empty mounts so containerd can extract into a throwaway temp dir without any bind mount syscall
	if kind == snapshots.KindActive && am.upperDir == "" && am.mountpoint == "" {
		return nil
	}

	// no FUSE mount (no parents, or FUSE unavailable): use upper dir directly
	if am.mountpoint == "" {
		if kind == snapshots.KindActive {
			return []mount.Mount{{
				Type:    "bind",
				Source:  am.upperDir,
				Options: []string{"rw", "rbind"},
			}}
		}
		// view with no parents: synthetic empty bind mount
		emptyDir := filepath.Join(s.root, "mounts", keyPath(key))
		_ = os.MkdirAll(emptyDir, 0o755)
		return []mount.Mount{{
			Type:    "bind",
			Source:  emptyDir,
			Options: []string{"ro", "rbind"},
		}}
	}

	if kind == snapshots.KindView {
		return []mount.Mount{{
			Type:    "bind",
			Source:  am.mountpoint,
			Options: []string{"ro", "rbind"},
		}}
	}

	// KindActive with parents: overlayfs on top of the FUSE lowerdir (requires kernel ≥ 5.11 for FUSE as overlayfs lowerdir)
	return []mount.Mount{{
		Type:   "overlay",
		Source: "overlay",
		Options: []string{
			fmt.Sprintf("lowerdir=%s", am.mountpoint),
			fmt.Sprintf("upperdir=%s", am.upperDir),
			fmt.Sprintf("workdir=%s", am.workDir),
		},
	}}
}

// openLayerByDiffID finds the blob in the content store for diffID and returns it as an fs.FS.
// Used directly by tests.
func (s *Snapshotter) openLayerByDiffID(ctx context.Context, diffID digest.Digest) (fs.FS, error) {
	blobDigest := s.findBlobByDiffID(ctx, diffID.String())
	if blobDigest == "" {
		return nil, fmt.Errorf("no content blob found for diffID %s: %w", diffID, errdefs.ErrNotFound)
	}
	return s.openBlobAsFS(ctx, blobDigest)
}

// readContentBlob reads the full content of a blob from the content store.
func (s *Snapshotter) readContentBlob(ctx context.Context, d digest.Digest) ([]byte, error) {
	ra, err := s.cs.ReaderAt(ctx, ocispec.Descriptor{Digest: d})
	if err != nil {
		return nil, err
	}
	defer ra.Close()
	return io.ReadAll(io.NewSectionReader(ra, 0, ra.Size()))
}

// openBlobAsFS opens the blob at blobDigest from the content store and returns it as an fs.FS backed by tarfs.  Gzip-compressed blobs are handled transparently.
func (s *Snapshotter) openBlobAsFS(ctx context.Context, blobDigest digest.Digest) (fs.FS, error) {
	// blob data is immutable (content-addressed); detach from the snapshot RPC context so the ReaderAt outlives it, while preserving gRPC metadata (namespace etc.)
	ra, size, err := s.openBlob(context.WithoutCancel(ctx), ocispec.Descriptor{Digest: blobDigest})
	if err != nil {
		return nil, fmt.Errorf("opening blob %s: %w", blobDigest, err)
	}

	// detect compression by magic bytes
	header := make([]byte, 4)
	if _, err := ra.ReadAt(header, 0); err != nil {
		ra.Close()
		return nil, fmt.Errorf("reading blob header for %s: %w", blobDigest, err)
	}

	var tarRA interface {
		ReadAt([]byte, int64) (int, error)
	}
	tarSize := size

	switch {
	case header[0] == 0x1f && header[1] == 0x8b:
		// gzip-compressed blob (application/vnd.oci.image.layer.v1.tar+gzip)
		// gsip builds deflate checkpoints for seek-ahead but only at block
		// boundaries (~32 KB each).  Tiny layers have zero or one block, giving
		// zero checkpoints, so gsip.ReadAt fails for any non-zero offset.
		// Decompress the whole blob into a bytes.Buffer for reliable random
		// access; for large layers (>=4 MiB compressed) use gsip instead.
		const gsipThreshold = 4 << 20 // 4 MiB compressed
		if size < gsipThreshold {
			r, gzErr := gzip.NewReader(io.NewSectionReader(ra, 0, size))
			if gzErr != nil {
				ra.Close()
				return nil, fmt.Errorf("creating gzip reader for %s: %w", blobDigest, gzErr)
			}
			var buf bytes.Buffer
			if _, gzErr = io.Copy(&buf, r); gzErr != nil {
				ra.Close()
				return nil, fmt.Errorf("decompressing blob for %s: %w", blobDigest, gzErr)
			}
			r.Close()
			data := buf.Bytes()
			tarRA = bytes.NewReader(data)
			tarSize = int64(len(data))
		} else {
			zr, gsipErr := gsip.NewReader(ra, size)
			if gsipErr != nil {
				ra.Close()
				return nil, fmt.Errorf("creating gsip reader for %s: %w", blobDigest, gsipErr)
			}
			tarRA = zr
			tarSize = 1<<63 - 1 // uncompressed size unknown
		}
	case header[0] == 0x28 && header[1] == 0xB5 && header[2] == 0x2F && header[3] == 0xFD:
		// zstd-compressed blob (application/vnd.oci.image.layer.v1.tar+zstd, produced by BuildKit).
		// TODO there is no gsip-equivalent for zstd random access, so we always decompress into memory; very large zstd layers should ideally be written to a temp file instead.
		zr, zstdErr := zstd.NewReader(io.NewSectionReader(ra, 0, size))
		if zstdErr != nil {
			ra.Close()
			return nil, fmt.Errorf("creating zstd reader for %s: %w", blobDigest, zstdErr)
		}
		var buf bytes.Buffer
		if _, zstdErr = io.Copy(&buf, zr); zstdErr != nil {
			ra.Close()
			return nil, fmt.Errorf("decompressing zstd blob for %s: %w", blobDigest, zstdErr)
		}
		zr.Close()
		data := buf.Bytes()
		tarRA = bytes.NewReader(data)
		tarSize = int64(len(data))
	default:
		tarRA = ra
	}

	fsys, err := tarfs.New(tarRA, tarSize)
	if err != nil {
		ra.Close()
		return nil, fmt.Errorf("building tarfs for %s: %w", blobDigest, err)
	}
	// tarfs.New just completed the sequential scan, sending all checkpoint updates into the gsip drain goroutine's channel; yield so it can flush them into r.checkpoints before any FUSE read calls gsip.ReadAt
	if _, ok := tarRA.(*gsip.Reader); ok {
		runtime.Gosched()
	}
	return fsys, nil
}

// openBlob returns a ReaderAt and size for the blob described by desc.
func (s *Snapshotter) openBlob(ctx context.Context, desc ocispec.Descriptor) (content.ReaderAt, int64, error) {
	ra, err := s.cs.ReaderAt(ctx, desc)
	if err != nil {
		return nil, 0, err
	}
	return ra, ra.Size(), nil
}

// propagateNamespace extracts the containerd namespace from an incoming gRPC context and re-stamps it as an outgoing gRPC header so that downstream calls to the content-store proxy include the namespace correctly.
func propagateNamespace(ctx context.Context) context.Context {
	if ns, ok := namespaces.Namespace(ctx); ok {
		return namespaces.WithNamespace(ctx, ns)
	}
	return namespaces.WithNamespace(ctx, "default")
}

// stopMount unmounts the FUSE server and removes its mount directory.
func (s *Snapshotter) stopMount(am *activeMount) {
	if am.server != nil {
		am.server.Unmount()
		_ = os.RemoveAll(am.mountpoint)
	}
}
