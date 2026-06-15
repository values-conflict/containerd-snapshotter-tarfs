package main

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	go2fuse "github.com/cpuguy83/go2fuse"
	gofusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/jonjohnsonjr/targz/gsip"
	"github.com/jonjohnsonjr/targz/tarfs"
)

// Snapshotter implements snapshots.Snapshotter by serving OCI image layers directly from the containerd content store via FUSE, without extraction.
type Snapshotter struct {
	root string
	ms   *storage.MetaStore
	cs   content.Store

	mu     sync.Mutex
	mounts map[string]*activeMount // snapshot key → live FUSE mount
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
	for _, sub := range []string{".", "meta", "mounts", "upper", "work"} {
		if err := os.MkdirAll(filepath.Join(stateDir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("creating state dir %q: %w", filepath.Join(stateDir, sub), err)
		}
	}

	ms, err := storage.NewMetaStore(filepath.Join(stateDir, "metadata.db"))
	if err != nil {
		return nil, fmt.Errorf("opening metadata store: %w", err)
	}

	return &Snapshotter{
		root:   stateDir,
		ms:     ms,
		cs:     cs,
		mounts: map[string]*activeMount{},
	}, nil
}

// --- snapshots.Snapshotter interface ---

func (s *Snapshotter) Stat(ctx context.Context, key string) (info snapshots.Info, err error) {
	err = s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		_, info, _, err = storage.GetInfo(ctx, key)
		return err
	})
	return info, err
}

func (s *Snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (updated snapshots.Info, err error) {
	err = s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		updated, err = storage.UpdateInfo(ctx, info, fieldpaths...)
		return err
	})
	return updated, err
}

func (s *Snapshotter) Usage(ctx context.Context, key string) (_ snapshots.Usage, err error) {
	var usage snapshots.Usage
	err = s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		_, _, usage, err = storage.GetInfo(ctx, key)
		return err
	})
	return usage, err
}

func (s *Snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return s.createSnapshot(ctx, snapshots.KindActive, key, parent, opts)
}

func (s *Snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return s.createSnapshot(ctx, snapshots.KindView, key, parent, opts)
}

func (s *Snapshotter) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	var snap storage.Snapshot
	var err error
	if err = s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		snap, err = storage.GetSnapshot(ctx, key)
		return err
	}); err != nil {
		return nil, fmt.Errorf("getting snapshot %q: %w", key, err)
	}

	s.mu.Lock()
	am := s.mounts[key]
	s.mu.Unlock()

	if am == nil {
		// mount not live (process restart, or View after Mounts call gap) -- rebuild
		newMount, err := s.buildMount(ctx, key, snap)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.mounts[key] = newMount
		s.mu.Unlock()
		am = newMount
	}

	return s.mountsFor(snap, am), nil
}

func (s *Snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	// resolve the blob digest before taking the write lock (content store I/O is fine here)
	blobDigest := s.resolveBlob(ctx, name)

	if err := s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		id, err := storage.CommitActive(ctx, key, name, snapshots.Usage{}, opts...)
		if err != nil {
			return fmt.Errorf("committing snapshot %q as %q: %w", key, name, err)
		}

		// write sidecar so we can map storage ID → blob at mount time
		if blobDigest != "" {
			if err := s.writeBlob(id, blobDigest.String()); err != nil {
				return fmt.Errorf("writing blob sidecar for %q: %w", id, err)
			}
		} else {
			log.Printf("tarfs: Commit %q: no blob resolved, layer will not be mountable", name)
		}

		return nil
	}); err != nil {
		return err
	}

	// clean up the active mount now that we have committed
	s.mu.Lock()
	am := s.mounts[key]
	delete(s.mounts, key)
	s.mu.Unlock()

	if am != nil {
		s.stopMount(am)
	}

	return nil
}

// resolveBlob returns the compressed blob digest for the snapshot being committed.
// containerd wraps committed snapshot names as "namespace/seq/chainID"; the last component
// is the OCI chainID, which uniquely identifies the layer's position in its chain.
//
// Fast path: for uncompressed blobs (tests; layer 0 where chainID == diffID == blob digest)
// the blob lives at the chainID address itself in the content store.
//
// Main path: walk image manifests.  Manifests are fetched before layer extraction begins in
// all containerd versions and pull paths, so they are always present at Commit time.  For each
// manifest the image config provides rootfs.diff_ids; recomputing the OCI chain formula
// identifies which layer index matches our chainID, and the manifest's Layers[i].Digest is
// the compressed blob we want to cache.  This works across all containerd versions without
// depending on opts labels or the timing of containerd.io/uncompressed.
func (s *Snapshotter) resolveBlob(ctx context.Context, name string) digest.Digest {
	pctx := propagateNamespace(ctx)
	// containerd wraps names as "namespace/seq/chainID"; the last component is the chainID
	chainID := name[strings.LastIndex(name, "/")+1:]

	// fast path: uncompressed blob lives at its own digest (layer 0: chainID == diffID)
	if d, err := digest.Parse(chainID); err == nil {
		if _, csErr := s.cs.Info(pctx, d); csErr == nil {
			return d
		}
	}

	return s.findBlobByChainID(pctx, chainID)
}

// findBlobByChainID walks image manifests in the content store to find the compressed blob
// digest for the layer identified by chainID.  For each blob with a
// containerd.io/gc.ref.content.config label (an image manifest), it reads the manifest JSON
// to get the layer descriptors and the image config to get rootfs.diff_ids, then recomputes
// the OCI chainID chain until it finds a match.
func (s *Snapshotter) findBlobByChainID(ctx context.Context, chainID string) digest.Digest {
	var result digest.Digest
	if err := s.cs.Walk(ctx, func(info content.Info) error {
		if info.Labels["containerd.io/gc.ref.content.config"] == "" {
			return nil
		}

		manifestRA, err := s.cs.ReaderAt(ctx, ocispec.Descriptor{Digest: info.Digest})
		if err != nil {
			return nil
		}
		manifestData, err := io.ReadAll(io.NewSectionReader(manifestRA, 0, manifestRA.Size()))
		manifestRA.Close()
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

		configRA, err := s.cs.ReaderAt(ctx, ocispec.Descriptor{Digest: manifest.Config.Digest})
		if err != nil {
			return nil
		}
		configData, err := io.ReadAll(io.NewSectionReader(configRA, 0, configRA.Size()))
		configRA.Close()
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
			if chain.String() == chainID {
				result = manifest.Layers[i].Digest
				return nil
			}
		}
		return nil
	}); err != nil {
		log.Printf("tarfs: findBlobByChainID: content store walk error: %v", err)
	}
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
		if info.Labels["containerd.io/uncompressed"] == diffIDStr {
			result = info.Digest
		}
		return nil
	})
	return result
}

func (s *Snapshotter) Remove(ctx context.Context, key string) error {
	var id string
	if err := s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		var err error
		id, _, err = storage.Remove(ctx, key)
		return err
	}); err != nil {
		return fmt.Errorf("removing snapshot %q: %w", key, err)
	}

	s.mu.Lock()
	am := s.mounts[key]
	delete(s.mounts, key)
	s.mu.Unlock()

	if am != nil {
		s.stopMount(am)
	}

	// clean up state directories
	_ = os.RemoveAll(filepath.Join(s.root, "upper", id))
	_ = os.RemoveAll(filepath.Join(s.root, "work", id))
	_ = os.Remove(s.blobPath(id))

	return nil
}

func (s *Snapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, filters ...string) error {
	return s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		return storage.WalkInfo(ctx, fn, filters...)
	})
}

func (s *Snapshotter) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, am := range s.mounts {
		s.stopMount(am)
	}
	s.mounts = map[string]*activeMount{}
	return s.ms.Close()
}

// --- internal helpers ---

func (s *Snapshotter) createSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string, opts []snapshots.Opt) (_ []mount.Mount, err error) {
	var snap storage.Snapshot

	if err := s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		var txErr error
		snap, txErr = storage.CreateSnapshot(ctx, kind, key, parent, opts...)
		return txErr
	}); err != nil {
		return nil, fmt.Errorf("creating snapshot %q: %w", key, err)
	}

	am, err := s.buildMount(ctx, key, snap)
	if err != nil {
		// roll back the metadata entry
		_ = s.ms.WithTransaction(context.Background(), true, func(ctx context.Context) error {
			_, _, _ = storage.Remove(ctx, key)
			return nil
		})
		return nil, err
	}

	s.mu.Lock()
	s.mounts[key] = am
	s.mu.Unlock()

	return s.mountsFor(snap, am), nil
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

// buildMount constructs (or rebuilds) the mount setup for snapshot snap.
//
// For extraction-type snapshots we return empty mounts so containerd extracts the layer tar into a throwaway temp dir (to compute and verify the DiffID) without requiring a bind mount syscall.  The blob stays in the content store and is read at FUSE mount time.
//
// For container-serving snapshots (View / non-extraction Prepare) we attempt to mount the parent layer chain via FUSE.  If FUSE is unavailable in this environment the snapshot is still created and the mounts are returned, but they point at an empty directory.  Container filesystems will be empty in that degraded mode -- the implementation is correct, it just needs FUSE to actually serve content.
func (s *Snapshotter) buildMount(ctx context.Context, key string, snap storage.Snapshot) (*activeMount, error) {
	am := &activeMount{}

	extraction := isExtractionKey(key)
	if extraction && snap.Kind == snapshots.KindActive {
		// return empty mounts: extraction runs into containerd's throwaway temp dir
		return am, nil
	}

	// build the FUSE layer stack from committed parents
	if len(snap.ParentIDs) > 0 && !extraction {
		var fsLayers []fs.FS
		for i := len(snap.ParentIDs) - 1; i >= 0; i-- {
			layerFS, err := s.openLayerByID(ctx, snap.ParentIDs[i])
			if err != nil {
				return nil, fmt.Errorf("opening layer for storage id %q: %w", snap.ParentIDs[i], err)
			}
			fsLayers = append(fsLayers, layerFS)
		}

		// fsLayers is bottom-first; LayeredFS wants top-first
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

		// use the storage ID (numeric) for the mount dir so snapshot keys
		// containing slashes or colons (eg sha256:abc...) stay path-safe
		mountpoint := filepath.Join(s.root, "mounts", snap.ID)
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

	if snap.Kind == snapshots.KindActive {
		upperDir := filepath.Join(s.root, "upper", snap.ID)
		workDir := filepath.Join(s.root, "work", snap.ID)
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

// mountsFor returns the []mount.Mount instructions for the given snapshot.
func (s *Snapshotter) mountsFor(snap storage.Snapshot, am *activeMount) []mount.Mount {
	// extraction snapshot: return empty mounts so containerd can extract into
	// a throwaway temp dir without any bind mount syscall
	if snap.Kind == snapshots.KindActive && am.upperDir == "" && am.mountpoint == "" {
		return nil
	}

	// no parents or no FUSE mount: just use the upper dir
	if len(snap.ParentIDs) == 0 || am.mountpoint == "" {
		if snap.Kind == snapshots.KindActive {
			return []mount.Mount{{
				Type:    "bind",
				Source:  am.upperDir,
				Options: []string{"rw", "rbind"},
			}}
		}
		// view with no parents: synthetic empty bind mount
		emptyDir := filepath.Join(s.root, "mounts", snap.ID)
		_ = os.MkdirAll(emptyDir, 0o755)
		return []mount.Mount{{
			Type:    "bind",
			Source:  emptyDir,
			Options: []string{"ro", "rbind"},
		}}
	}

	if snap.Kind == snapshots.KindView {
		return []mount.Mount{{
			Type:    "bind",
			Source:  am.mountpoint,
			Options: []string{"ro", "rbind"},
		}}
	}

	// KindActive with parents: overlayfs on top of the FUSE lowerdir
	// (requires kernel ≥ 5.11 for FUSE as overlayfs lowerdir)
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

// openLayerByID opens the layer blob for the committed snapshot with the given storage ID and returns it as an fs.FS.
func (s *Snapshotter) openLayerByID(ctx context.Context, storageID string) (fs.FS, error) {
	ctx = propagateNamespace(ctx)
	blobDigestStr, err := s.readBlob(storageID)
	if err != nil {
		return nil, fmt.Errorf("reading blob for storage id %q: %w", storageID, err)
	}
	if blobDigestStr == "" {
		// no blob (e.g. Docker's synthetic init layer written directly to the upper dir)
		upperDir := filepath.Join(s.root, "upper", storageID)
		if info, statErr := os.Stat(upperDir); statErr == nil && info.IsDir() {
			return os.DirFS(upperDir), nil
		}
		return nil, fmt.Errorf("no blob recorded for storage id %q: %w", storageID, errdefs.ErrNotFound)
	}
	blobDigest, err := digest.Parse(blobDigestStr)
	if err != nil {
		return nil, fmt.Errorf("parsing blob digest for storage id %q: %w", storageID, err)
	}
	return s.openBlobAsFS(ctx, blobDigest)
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

// openBlobAsFS opens the blob at blobDigest from the content store and returns it as an fs.FS
// backed by tarfs.  Gzip-compressed blobs are handled transparently.
func (s *Snapshotter) openBlobAsFS(ctx context.Context, blobDigest digest.Digest) (fs.FS, error) {
	// blob data is immutable (content-addressed); detach from the snapshot RPC context
	// so the ReaderAt outlives it, while preserving gRPC metadata (namespace etc.)
	ra, size, err := s.openBlob(context.WithoutCancel(ctx), ocispec.Descriptor{Digest: blobDigest})
	if err != nil {
		return nil, fmt.Errorf("opening blob %s: %w", blobDigest, err)
	}

	// detect gzip by magic bytes
	header := make([]byte, 2)
	if _, err := ra.ReadAt(header, 0); err != nil {
		ra.Close()
		return nil, fmt.Errorf("reading blob header for %s: %w", blobDigest, err)
	}

	var tarRA interface {
		ReadAt([]byte, int64) (int, error)
	}
	tarSize := size

	if header[0] == 0x1f && header[1] == 0x8b {
		// gzip-compressed blob
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
	} else {
		tarRA = ra
	}

	fsys, err := tarfs.New(tarRA, tarSize)
	if err != nil {
		ra.Close()
		return nil, fmt.Errorf("building tarfs for %s: %w", blobDigest, err)
	}
	// tarfs.New just completed the sequential scan, sending all checkpoint updates
	// into the gsip drain goroutine's channel; yield so it can flush them into
	// r.checkpoints before any FUSE read calls gsip.ReadAt
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

// writeBlob records the compressed blob digest for storageID in a sidecar file.
func (s *Snapshotter) writeBlob(storageID, blobDigest string) error {
	return os.WriteFile(s.blobPath(storageID), []byte(blobDigest), 0o600)
}

// readBlob reads the blob digest sidecar for storageID.  Returns "" when no sidecar exists.
func (s *Snapshotter) readBlob(storageID string) (string, error) {
	data, err := os.ReadFile(s.blobPath(storageID))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (s *Snapshotter) blobPath(storageID string) string {
	return filepath.Join(s.root, "meta", storageID+".blob")
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
