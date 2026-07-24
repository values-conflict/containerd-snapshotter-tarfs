# containerd-snapshotter-tarfs

A FUSE-based containerd proxy snapshotter that mounts OCI image layers directly from the content store.

## Caveat: extraction still happens at pull time

Despite the goal of skipping extraction entirely, tarfs currently _does_ extract each layer tarball during `docker pull` -- just to a temporary directory that's immediately deleted.

The culprit is a coupling baked into containerd's unpack pipeline.  The transfer service fetches manifests and configs, but layer blobs are downloaded by the per-layer unpacker goroutine, which also applies the layer to the snapshot.  The proxy snapshotter API has no signal for "fetch the blob but skip the apply": returning `snapshots.ErrAlreadyExists` from `Prepare` skips both the apply _and_ the fetch, so the blob never lands in the content store and tarfs has nothing to serve from.

The current workaround is to return empty mounts from `Prepare` for extraction-keyed snapshots.  containerd then extracts into a temporary directory under `/var/lib/containerd/tmpmounts/`, computes the diffID from the decompressed stream, and -- with a patched containerd (see next section) -- deletes the temporary directory when done.  The compressed blob stays in the content store; the FUSE mount serves all subsequent reads from it.

Practical impact:

- **At rest:** only compressed blobs on disk, no extracted copies -- the disk-space goal is met
- **During pull:** the full uncompressed layer is temporarily written to and immediately deleted from disk; for a golang-sized image (~300 MiB compressed, ~800 MiB uncompressed) this means pull I/O is roughly 2.7x a bare download
- **Container start:** reads come from the FUSE mount backed by the compressed blob, as intended

The real fix requires decoupling fetch from extraction inside containerd.  A custom differ plugin registered within the containerd process could fetch without extracting, but proxy snapshotters can't reach the containerd plugin registry.  No upstream API for "fetch-only" applies exists today.  Until one does, every image pull discards roughly 2-3x the image's compressed size in temporary disk I/O.

## Caveat: the temp dir cleanup requires a patched containerd

This doesn't fix the fundamental problem -- extraction still happens in full; it just ensures the extracted content is actually deleted afterward rather than accumulating on disk forever.

Stock containerd's [`WithTempMount`](https://github.com/containerd/containerd/blob/v2.3.2/core/mount/temp.go#L42-L53) cleans up the temp directory with `os.Remove`, which returns `ENOTEMPTY` on a non-empty directory and does nothing further -- the error is logged and swallowed.  Since the extraction callback wrote the full uncompressed layer into the directory before cleanup ran, `os.Remove` always fails, and the extracted content accumulates indefinitely under `/var/lib/containerd/tmpmounts/`.  Without the patch, every `docker pull` permanently leaves the full uncompressed layer on disk rather than just transiently.

The fix:

```diff
--- a/core/mount/temp.go
+++ b/core/mount/temp.go
@@ -45,9 +45,18 @@ func WithTempMount(ctx context.Context, mounts []Mount, f func(root string) erro
 	// the mounted dir. However, if we use Remove, even though we won't
 	// successfully delete the temp dir and it may leak, we won't loss data
 	// from the mounted dir.
 	// For details, please refer to #1868 #1785.
+	//
+	// When no mounts were attempted, nothing is mounted under root, so
+	// RemoveAll is safe as a fallback if Remove fails (e.g. when the callback
+	// wrote files directly into root with no prior mount syscall).
 	defer func() {
 		if uerr = os.Remove(root); uerr != nil {
-			log.G(ctx).WithError(uerr).WithField("dir", root).Error("failed to remove mount temp dir")
+			if len(mounts) == 0 {
+				uerr = os.RemoveAll(root)
+			}
+			if uerr != nil {
+				log.G(ctx).WithError(uerr).WithField("dir", root).Error("failed to remove mount temp dir")
+			}
 		}
 	}()
```

When `len(mounts) == 0`, no mount syscall was made, so nothing is live in the directory and `os.RemoveAll` is safe.  This should be submitted upstream; until it lands in a released containerd, you'll need to build from source.

## Why?

The standard containerd snapshotters (`overlayfs`, `native`) extract each layer's tar into a separate directory tree.  For large images or read-heavy workloads this wastes both disk space and startup time.  The goal here is to skip extraction entirely and serve the filesystem directly from the compressed blobs already sitting in the content store.

## Building

```console
$ go build -o containerd-snapshotter-tarfs .
```

## Usage

```console
$ containerd-snapshotter-tarfs [flags]
```

Flags:

- `--socket` -- unix socket path for the snapshotter gRPC server (default: `/run/containerd-snapshotter-tarfs/snapshotter.sock`)
- `--state-dir` -- directory for snapshotter metadata and FUSE mount points (default: `/var/lib/containerd-snapshotter-tarfs`)
- `--containerd-socket` -- containerd gRPC socket, for content store access (default: `/run/containerd/containerd.sock`)

## Configuring containerd

Add the following to your `config.toml` (typically `/etc/containerd/config.toml`):

```toml
[proxy_plugins.tarfs]
  type = "snapshot"
  address = "/run/containerd-snapshotter-tarfs/snapshotter.sock"

[[plugins."io.containerd.transfer.v1.local".unpack_config]]
  snapshotter = "tarfs"
  platform = "linux"
```

The `unpack_config` entry is required so that the transfer service sets `containerd.io/snapshot/diff-id` labels during image pulls -- without it, layer blobs can't be resolved back to their content store entries.  The `platform` field cannot be omitted (an empty string fails to parse), but `"linux"` matches all Linux architectures since `check_platform_supported` defaults to `false`, which means only the OS is checked -- cross-architecture pulls (`--platform linux/arm64` on an amd64 host, etc.) work correctly.

containerd v2.0.x predates `check_platform_supported`, so `platform = "linux"` only matches the host architecture there and cross-architecture pulls fail.  The workaround is to replace the single entry with one per architecture, using `differ = "walking"` on any entry whose platform does not match the host (since v2.0.x selects the differ by matching the configured platform against the host's registered differs, and specifying the differ by name bypasses that check):

```toml
[[plugins."io.containerd.transfer.v1.local".unpack_config]]
  snapshotter = "tarfs"
  platform = "linux/amd64"

[[plugins."io.containerd.transfer.v1.local".unpack_config]]
  snapshotter = "tarfs"
  platform = "linux/arm64"
  differ = "walking"
```

After updating the config, start `containerd-snapshotter-tarfs` before (or alongside) `containerd`, then pull and run images using `--snapshotter tarfs`:

```console
$ ctr images pull --snapshotter tarfs docker.io/library/hello-world:latest
$ ctr images mount --snapshotter tarfs docker.io/library/hello-world:latest /mnt/hello-world
```

## Configuring Docker

Add to `/etc/docker/daemon.json`:

```json
{
	"containerd": "/run/containerd/containerd.sock",
	"features": {
		"containerd-snapshotter": true
	},
	"storage-driver": "tarfs"
}
```

- `"containerd"` -- points Docker at the same containerd instance where tarfs is registered; omit if Docker already connects to `/run/containerd/containerd.sock` via its service unit (specifying the same value in both places causes a startup error)
- `"features": {"containerd-snapshotter": true}` -- enables the containerd image store; without it, `"storage-driver"` is treated as a graph driver name and fails
- `"storage-driver": "tarfs"` -- selects tarfs as Docker's default snapshotter

Restart Docker _after_ `containerd-snapshotter-tarfs` is running and containerd has tarfs in its proxy plugins.  Docker queries containerd for available snapshotters at startup -- if tarfs isn't registered yet, Docker won't find it.

Once configured, `docker pull` and `docker run` use tarfs automatically (Docker doesn't expose a per-command `--snapshotter` flag):

```console
$ docker pull docker.io/library/bash:latest
$ docker run --rm docker.io/library/bash:latest bash --version
```

## Notes

FUSE mounts require `CAP_SYS_ADMIN` (or a user namespace with `--map-root-user`).  In a dev environment without root, wrap all processes in a shared namespace:

```console
$ unshare --user --map-root-user --mount -- bash
```

All processes that need to share FUSE mounts (`containerd`, `containerd-snapshotter-tarfs`, `ctr`) must run inside the _same_ namespace -- each separate `unshare` call creates a distinct mount namespace.
