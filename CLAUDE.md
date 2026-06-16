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
1. Extract the topChainID from the parent snapshot's backend key (last `"/"` component)
2. Call `buildLayerStack(topChainID)` to find all layer blobs from base to topChainID
3. Open each blob: gzip layers via `gsip.NewReader` or `bytes.Buffer`; plain tarballs directly
4. Feed each `io.ReaderAt` into `tarfs.New` to get a seek-capable `fs.FS` over the tar entries
5. Stack the per-layer `fs.FS` instances into a `LayeredFS` that implements whiteout semantics
6. Mount the `LayeredFS` as a FUSE filesystem via `cpuguy83/go2fuse` (which adapts `io/fs.FS` to hanwen/go-fuse)
7. Return the FUSE mount point as a bind mount (View) or as the lowerdir of an overlayfs mount (Active/writable)

**Snapshot kinds:**
- `Prepare` (writable active) -- calls `buildLayerStack`, mounts FUSE, creates `upper/` and `work/` dirs, returns `type: overlay` with FUSE as lowerdir.  Extraction snapshots return empty mounts so containerd can extract into a throwaway temp dir.
- `View` (read-only active) -- calls `buildLayerStack`, mounts FUSE, returns a `ro` bind mount.
- `Commit` -- records kind and parent chain in memory; propagates chain info for Docker init-layer continuity.

**Layer→blob mapping** (`buildLayerStack`): walks content-store blobs labeled `containerd.io/gc.ref.content.config` (image manifests), parses each manifest's config `rootfs.diff_ids`, recomputes the OCI chain formula (`chainID[i] = sha256(chainID[i-1] + " " + diffID[i])`) to find which layer index matches topChainID, and collects all layer blobs from base up to that index.  Found blobs are labeled `tianon.xyz/values-conflict/tarfs/chain-id = chainID` for fast future lookup.  Three-pass fallback: labeled manifests → all blobs → chainID formula (for docker commit/build layers whose manifests aren't labeled).

**FUSE mount lifecycle:**
- Mounts live at `<state-root>/mounts/<sha256(key)>/`
- Active mounts tracked in-process; View/Active snapshots do NOT survive process restarts (FUSE mounts are gone, FUSE directories are stale)
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

**`containerd.io/uncompressed` label:** this label is set on the compressed blob by the unpacker after successful extraction.  `buildLayerStack` uses the manifest walk (not this label) to find layer blobs, which avoids depending on extraction timing.  The label IS used by `findBlobByDiffID` (the test path for uncompressed blobs) and by `findNewLayerBlob` (the chainID-formula fallback for docker commit/build layers).

**BuildKit snapshot ordering (Commit-before-diff):** BuildKit calls `Commit` on our proxy BEFORE computing the diff blob for the new image layer.  After Commit, BuildKit creates a `View` of the committed snapshot and diffs it against the parent to produce the layer tar.  Our Commit was calling `stopMount` (unmounting FUSE), so the View fell back to serving only the parent's OCI layers -- the diff came out empty, and the new layer blob had no files.  Fix: preserve the active snapshot's `upperDir` in `upperDirs` at Commit time so that any subsequent View of the committed snapshot includes the new files as the top layer.  The classic builder (`DOCKER_BUILDKIT=0`) does NOT have this issue because it computes the diff from the overlay's upper dir directly before calling Commit.

**BuildKit layer compression:** BuildKit produces zstd-compressed layers (`application/vnd.oci.image.layer.v1.tar+zstd`, magic `0x28 0xB5 0x2F 0xFD`) while the classic builder produces gzip.  Failing to detect zstd causes `tarfs.New` to silently parse the compressed frame as an uncompressed tar, returning an empty FS with no error.

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

`gsip.NewReader` builds deflate checkpoints asynchronously in a goroutine.  For small gzip blobs (< 4 MiB compressed) there is typically only one deflate block and zero checkpoints, making `ReadAt` fail for any non-zero offset.  `openBlobAsFS` decompresses these blobs entirely into a `bytes.Buffer`.  Blobs ≥ 4 MiB use gsip with a `runtime.Gosched()` call after `tarfs.New` to let the checkpoint goroutine drain.

## Content store access

The proxy snapshotter must connect back to containerd's gRPC socket (not directly open the metadata BoltDB) to read content store labels like `containerd.io/uncompressed`.  Direct BoltDB access would block on containerd's exclusive flock.  The gRPC content proxy requires the containerd namespace in the outgoing gRPC context; `propagateNamespace(ctx)` converts the incoming namespace from the snapshot RPC into an outgoing header for content store calls.

## Current status (2026-06-16)

**Phase:** working end-to-end in CI across all supported containerd versions; `docker run`, `docker build` (classic and BuildKit), and `docker commit` all pass.

**Done:**
- `LayeredFS` with correct OCI whiteout semantics; unit tests pass
- `TestFUSEMount_singleLayer` passes under `unshare --user --map-root-user --mount`
- `openLayerByDiffID` with fast path (uncompressed blob at diffID address) and manifest-walk path; integration tests pass
- Empty-mount extraction: avoids bind mount EPERM, lets blobs stay in content store
- gsip small-blob fix: blobs < 4 MiB decompressed to `bytes.Buffer` for reliable random access
- BoltDB/MetaStore eliminated entirely: no `metadata.db`; snapshot kinds and parent chains are in-memory; layer→blob mapping lives as `tianon.xyz/values-conflict/tarfs/chain-id` labels on content-store blobs
- `buildLayerStack` derives the full ordered layer stack from content-store manifests; three-pass fallback (labeled manifests → all blobs → chainID formula) covers normal pulls, `docker commit`, and BuildKit builds
- Docker daemon integration: `docker run`, `docker build` (classic and BuildKit), `docker commit`, cross-arch runs all pass in CI
- `bash:latest`, cross-arch `bash:latest` (arm64), and various `tianon/test:badtars-*` images all tested

**Known limitations:**
- Writable containers still use `type: overlay` with the FUSE directory as lowerdir; eliminating this requires a writable FUSE implementation (see `TODO.md`)
- View/Active snapshot state is in-memory only -- containers and image mounts do not survive snapshotter process restarts

## Maintaining this file

Update the **Current status** section whenever the shape of the project changes -- when a milestone is reached, when a design decision changes, or when a new blocker appears.  Keep the **Done** / **Next** lists current.  If the architecture section diverges from the implementation, fix the architecture section to match.  This file is the primary handoff document between sessions.
