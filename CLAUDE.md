# containerd-snapshotter-tarfs

A FUSE-based snapshotter for containerd that mounts container image layers directly from the content store as live tar filesystems -- no extraction step, no duplicate disk usage.

## Style

This is a Tianon-maintained repository -- [tianonfmt style guides](https://github.com/values-conflict/tianonfmt/tree/cc3b1f1b172d6e35c773a19ab8b12733105730a8/docs) apply as authoritative for all file types.  Prose mode is **Cute/Lenient** (not Strict): the curated set of Unicode characters documented in [prose.md](https://github.com/values-conflict/tianonfmt/blob/cc3b1f1b172d6e35c773a19ab8b12733105730a8/docs/prose.md) is acceptable to leave in place.

## Why?

The standard containerd snapshotters (overlay, native) extract each layer's tar into a separate directory tree.  For large images or read-heavy workloads this wastes both disk space and startup time.  The goal here is to skip extraction entirely and serve the filesystem directly from the compressed blobs already sitting in the content store.

## Goals

- Mount OCI image layers directly from the containerd content store without extracting them to disk
- Support arbitrary layer depth (the full parent-chain of any image)
- Implement the whiteout semantics (`.wh.<name>` and `.wh..wh..opq`) required by the OCI layer spec so upper layers correctly shadow lower ones
- Register as a proxy snapshotter so containerd can delegate to it over a Unix socket
- Keep the implementation self-contained: one binary, one Unix socket, no kernel modules
- Support containerd proxy snapshotter clients from v1.7.x through current (v2.x) — the gRPC proxy protocol is stable across versions

## Architecture

**Package layout:** all Go source lives at the module root (`package main`), giving us a single binary.

**Core pipeline:** for each snapshot view/mount request, we:
1. Walk the committed parent chain in the BoltDB metadata store to collect the ordered list of layer storage IDs
2. Map each storage ID → diffID via the per-snapshot `.diffid` sidecar files we write at `Commit` time
3. Find the corresponding content-store blob via `content.Manager.Walk` (matching `containerd.io/uncompressed == diffID`) -- or use the diffID directly if the blob is uncompressed
4. Open gzip-compressed blobs via `gsip.NewReader` (the random-access gzip reader from `jonjohnsonjr/targz`) and plain tarballs directly
5. Feed each `io.ReaderAt` into `tarfs.New` to get a seek-capable `fs.FS` over the tar entries
6. Stack the per-layer `fs.FS` instances into a `LayeredFS` that implements whiteout semantics
7. Mount the `LayeredFS` as a FUSE filesystem via `cpuguy83/go2fuse` (which adapts `io/fs.FS` to hanwen/go-fuse)
8. Return the FUSE mount point as a bind mount to containerd

**Snapshot kinds:**
- `Prepare` (writable active) -- creates a tmpdir for the upper layer and, if a parent exists, starts a FUSE mount for the read-only parent chain.  Returns an overlayfs mount using the FUSE directory as lowerdir.  Writes during extraction go to the tmpdir and are ignored at `Commit` time; the content-store blob is the source of truth for future reads.
- `View` (read-only active) -- starts a FUSE mount for the full parent chain and returns a bind mount with `ro`.
- `Commit` -- records the snapshot's `containerd.io/snapshot/diff-id` label into a `<storage-id>.diffid` sidecar file, then calls `storage.CommitActive`.

**DiffID sidecar files** live at `<state-root>/meta/<storage-id>.diffid`.  They bridge the gap between the BoltDB parent-chain storage IDs (opaque integers) and the content-store blob lookup -- without these we would need an O(n) BoltDB Walk for every mount request.

**FUSE mount lifecycle:**
- Mounts live at `<state-root>/mounts/<snapshot-key>/`
- Active mounts are tracked in an in-process map; on restart the process remounts any views/actives that are still live in the metadata DB
- `Remove` unmounts and removes the mount directory

## Key dependencies

- `github.com/containerd/containerd/v2` -- snapshots storage, content store, GRPC proxy server
- `github.com/cpuguy83/go2fuse` -- bridges `io/fs.FS` to `hanwen/go-fuse` FUSE nodes
- `github.com/jonjohnsonjr/targz` (`tarfs` + `gsip` sub-packages) -- tar-as-FS with random-access gzip
- `github.com/hanwen/go-fuse/v2` -- low-level Linux FUSE implementation

## `.tmp/` directory

Local reference clones (or symlinks) used during development for code reading.  They are **not** used as Go module replacements; the `go.mod` references published versions.  These are listed in `.gitignore`.

If you already have local checkouts, **symlinks are preferred** -- you get a full clone with history and avoid redundant disk usage:

```console
$ mkdir -p .tmp
$ ln -s /path/to/go2fuse .tmp/go2fuse
$ ln -s /path/to/targz .tmp/targz
$ ln -s /path/to/dagdotdev .tmp/dagdotdev
$ ln -s /path/to/containerd .tmp/containerd
```

Otherwise, clone with `--depth=1`:

```console
$ mkdir -p .tmp
$ git clone --depth=1 https://github.com/cpuguy83/go2fuse .tmp/go2fuse
$ git clone --depth=1 https://github.com/jonjohnsonjr/targz .tmp/targz
$ git clone --depth=1 https://github.com/jonjohnsonjr/dagdotdev .tmp/dagdotdev
$ git clone --depth=1 https://github.com/containerd/containerd .tmp/containerd
```

**What each clone provides:**
- `.tmp/go2fuse` -- the `go2fuse` adapter source; useful for understanding how `io/fs.FS` maps to FUSE inodes and for adapting our `LayeredFS` to fit its `readLinkFS` interface expectations
- `.tmp/targz` -- `tarfs` and `gsip` source; the `tarfs.FS` type and `gsip.Reader` are the innermost layer of our stack
- `.tmp/dagdotdev` -- `pkg/soci` contains a `MultiFS` and whiteout-aware `ReadDir`; used as inspiration for the `LayeredFS` whiteout logic in `layers.go`
- `.tmp/containerd` -- containerd source; interface and type definitions for the snapshotter/content-store APIs; run `make` inside this directory to produce the `bin/containerd` and `bin/ctr` binaries used in *How to run* below

## Dependency notes

The containerd v2 project splits its API (gRPC-generated protobuf stubs) into a separate Go module: `github.com/containerd/containerd/api` (currently at v1.11.1 independently versioned).  Our `main.go` imports `github.com/containerd/containerd/api/services/snapshots/v1` for the `RegisterSnapshotsServer` call, while the core types (`snapshots.Snapshotter`, `content.Store`, `mount.Mount`) come from `github.com/containerd/containerd/v2`.

## Hard-won lessons (debugging notes)

**containerd metadata key transformation:** the metadata wrapper transforms snapshot keys before forwarding them to the backend -- `"extract-uuid chainID"` becomes `"default/<seq>/extract-uuid chainID"`.  `isExtractionKey` must check the LAST `/`-separated component, not the whole key.

**Transfer service is required for diffID labels:** the `--local` flag in `ctr images pull` uses the old `applyLayers` code path which does NOT set `containerd.io/snapshot/diff-id` in the Prepare opts.  The transfer service (default path, without `--local`) DOES set them.  To use the transfer service, the snapshotter must be registered in `[plugins.'io.containerd.transfer.v1.local'.unpack_config]`.

**Layer blobs and the unpack pipeline:** the transfer service's `fetchHandler` fetches manifests and configs but NOT layer blobs.  Layer blobs are fetched inside the unpacker goroutine.  If we skip extraction entirely (via skipExtraction returning ErrAlreadyExists), the layer blobs are never fetched to the content store.  The workaround is to let extraction run but return EMPTY mounts -- the extraction then writes to a throwaway temp dir, the DiffID is computed correctly, and the blob stays in the content store.

**Empty-mount extraction:** returning `[]mount.Mount{}` from Prepare for extraction keys is the correct approach.  `archive.Apply` with empty mounts creates its own temp dir for extraction.  The DiffID verification passes because it's computed from the tar stream, not from the directory.  This avoids both the bind mount EPERM and any need to skip blob fetching.

**Extraction EPERM (lchown):** in non-root environments, `archive.Apply` fails when the tar has UID=0 entries because `os.Lchown(path, 0, 0)` returns EPERM.  `archive.WithNoSameOwner()` would fix it but there's no way to inject it from outside the applier.  The `skipExtraction` approach (pre-commit without running extraction) avoids this but also skips the blob fetch.  Resolution: needs root or CAP_SYS_ADMIN for real image pulls; integration tests use manually-ingested blobs.

**`containerd.io/uncompressed` label:** this label is set on the compressed blob by the unpacker AFTER successful extraction.  It cannot be used in `openLayerByDiffID` as a fast path because extraction hasn't happened when we first call it.  We fall back to `findCompressedBlob` which walks image manifests using their `containerd.io/gc.ref.content.l.N` → config → `rootfs.diff_ids` chain.

**gsip and random-access reads:** `gsip.NewReader` builds gzip checkpoints asynchronously via a goroutine.  `tarfs.New` triggers the initial sequential scan.  The goroutine may not finish receiving all checkpoints before `ReadAt` is called for random access.  For tests, use uncompressed tar blobs (diffID == blob digest, no gsip needed) to test the fast path.  Real production images (gzip layers) work correctly as long as the sequential scan completes before concurrent reads begin -- which is the normal FUSE mount case.

## FUSE and namespace setup

Running in the dev container requires entering a user+mount namespace so that:
- `syscall.Mount` is allowed (CAP_SYS_ADMIN inside the namespace)
- `lchown` to uid=0 works (uid 1000 maps to uid=0 inside the namespace)
- FUSE mounts are private to the namespace (avoiding host privilege requirements)

The anchor for all processes is `unshare --user --map-root-user --mount`.  ALL processes that need to share FUSE mounts (containerd, the snapshotter, ctr) must run inside the SAME namespace -- each separate `unshare` call creates a different mount namespace and FUSE mounts do not propagate across them.

**How to run:**
```console
$ (cd .tmp/containerd && make)  # build containerd and ctr if not already built
$ unshare --user --map-root-user --mount -- bash
(in namespace) $ .tmp/containerd/bin/containerd --config /tmp/containerd-base.toml &
(in namespace) $ ./containerd-snapshotter-tarfs --containerd-socket ... &
(in namespace) $ .tmp/containerd/bin/ctr images pull --snapshotter tarfs ...
(in namespace) $ .tmp/containerd/bin/ctr images mount --snapshotter tarfs ...
```

**How to run tests:**
```console
$ go test -exec "unshare --user --map-root-user --mount" ./...
```

## gsip (gzip random-access) notes

`gsip.NewReader` builds deflate checkpoints asynchronously in a goroutine.  For small gzip blobs (< 4 MiB compressed) there is typically only one deflate block and zero checkpoints, making `ReadAt` fail for any non-zero offset.  We decompress these blobs entirely into a `bytes.Buffer` in `openLayerByDiffID`.  Blobs ≥ 4 MiB use gsip with a `runtime.Gosched()` call after `tarfs.New` to let the checkpoint goroutine drain.

## Content store access

The proxy snapshotter must connect back to containerd's gRPC socket (not directly open the metadata BoltDB) to read content store labels like `containerd.io/uncompressed`.  Direct BoltDB access would block on containerd's exclusive flock.  The gRPC content proxy requires the containerd namespace in the outgoing gRPC context; `propagateNamespace(ctx)` converts the incoming namespace from the snapshot RPC into an outgoing header for content store calls.

## Current status (2026-06-11)

**Phase:** core logic working and tested; `tianon/true:oci` works end-to-end; `bash:latest` blocked by GID mapping limitation in dev environment.

**Done:**
- Cloned reference repos into `.tmp/`
- Full `Snapshotter` implementation with gRPC proxy server and BoltDB metadata
- `LayeredFS` with correct OCI whiteout semantics; 4 unit tests pass
- `findCompressedBlob` walks image manifests to resolve diffID → compressed blob without the `containerd.io/uncompressed` label (which isn't set when we skip extraction)
- `openLayerByDiffID` fast path (uncompressed blob at diffID address), labeled-blob path, and manifest-walk path; 3 integration tests pass
- `TestOpenLayerByDiffID_multiLayer` verifies end-to-end: content store → tarfs → LayeredFS with whiteouts
- Binary compiles and gRPC server starts; containerd connects to our proxy socket and sees `io.containerd.snapshotter.v1.tarfs` plugin
- Transfer service unpack config properly wired in `containerd-base.toml` (`[[plugins.'io.containerd.transfer.v1.local'.unpack_config]]`)
- Image pulls reach our Prepare correctly with diffID labels set
- Empty-mount extraction approach verified: avoids bind mount EPERM and lets blobs be fetched
- **`tianon/true:oci` pulls, mounts, and serves files via FUSE correctly** -- `/true` executes with exit 0, ELF magic bytes verified, file listing correct
- gsip small-blob fix: blobs < 4 MiB are decompressed to `bytes.Buffer` for reliable random access
- FUSE test (`TestFUSEMount_singleLayer`) passes under `unshare --user --map-root-user --mount`
- Content store access via gRPC proxy with namespace propagation

**Blocked in dev environment (not in production):**
- `bash:latest` extraction fails: `/etc/shadow` has GID=42 (shadow group), which is not in the single-pair user namespace GID map (`--map-root-user` maps only GID 0).  Multi-GID mapping requires `newuidmap`/`newgidmap` (not installed) or root.
- Overlayfs with a FUSE lowerdir (for writable container roots) not yet tested

**Next steps:**
- Test `bash:latest` on a host with proper UID/GID subordinate mappings or root access
- Add gsip index caching (`<state-root>/gsip-index/<digest>`) for restart performance
- Add tarfs TOC caching to speed up FUSE mount creation after restart
- Write a `run-in-namespace.sh` wrapper script to simplify the `unshare` setup
- Consider a `--no-extract` flag that uses skipExtraction when blobs are already in the content store (for pre-seeded scenarios)

## Maintaining this file

Update the **Current status** section whenever the shape of the project changes -- when a milestone is reached, when a design decision changes, or when a new blocker appears.  Keep the **Done** / **Next** lists current.  If the architecture section diverges from the implementation, fix the architecture section to match.  This file is the primary handoff document between sessions.
