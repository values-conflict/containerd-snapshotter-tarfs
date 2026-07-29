# containerd-snapshotter-tarfs

A FUSE-based containerd proxy snapshotter that mounts OCI image layers directly from the content store -- no extraction step, no duplicate disk usage.

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
- `--state-dir` -- runtime-only state directory for FUSE mount points and overlay upper/work dirs; not preserved across restarts (default: `/run/containerd-snapshotter-tarfs`)
- `--containerd-socket` -- containerd gRPC socket, for content store access (default: `/run/containerd/containerd.sock`)

## Configuring containerd

Add the following to your `config.toml` (typically `/etc/containerd/config.toml`):

```toml
[proxy_plugins.tarfs]
  type    = "snapshot"
  address = "/run/containerd-snapshotter-tarfs/snapshotter.sock"

[proxy_plugins.tardiffs]
  type    = "diff"
  address = "/run/containerd-snapshotter-tarfs/snapshotter.sock"

[plugins."io.containerd.service.v1.diff-service"]
  default = ["tardiffs", "walking"]

[[plugins."io.containerd.transfer.v1.local".unpack_config]]
  snapshotter = "tarfs"
  differ      = "tardiffs"
  platform    = "linux"
```

Four entries are needed: two register the proxy plugins themselves (`[proxy_plugins.tarfs]` as a snapshot plugin, `[proxy_plugins.tardiffs]` as a differ), and two configure the per-pull-path routing.

**`docker pull`** (with `containerd-snapshotter: true`) calls `client.Pull()` → `unpack.Unpacker` → `io.containerd.service.v1.diff-service`.  It never consults `unpack_config`.  The `[plugins."io.containerd.service.v1.diff-service"]` stanza puts `tardiffs` first in the differ list so Docker's layer extraction calls land on our proxy differ.  Because the diff-service list is global (no per-snapshotter routing), `tardiffs` returns `codes.Unimplemented` for any Apply with non-empty mounts -- that signals the diff service to fall through to the walking differ, leaving overlayfs, CRI, and any other non-tarfs snapshotter unaffected.  The tarfs snapshotter always returns nil mounts for extraction-keyed snapshots, so zero-mount calls are always tarfs extractions.

**`ctr images pull`** (without `--local`) calls `client.Transfer()` instead, which goes through the transfer service and reads `unpack_config`.  The `[[plugins."io.containerd.transfer.v1.local".unpack_config]]` entry covers this path.  It also sets `containerd.io/snapshot/diff-id` labels on each snapshot so that layer blobs can be resolved back to their content store entries.

If `docker pull` used the transfer service (or if the diff service had per-snapshotter routing), only one routing entry would be needed.  It does not, and it does not.

The `platform` field in `unpack_config` cannot be omitted -- `platforms.Parse("")` hard-errors at containerd startup, and `"*"` is explicitly rejected.  `"linux"` without an explicit architecture fills in `runtime.GOARCH` when parsed, but `check_platform_supported` defaults to `false`, which switches snapshot-selection from `platforms.Only` (exact OS and architecture) to `platforms.OnlyOS` (OS only) -- so `"linux"` covers `linux/amd64`, `linux/arm64`, and all other Linux architectures without listing each.  Note: `differ = "tardiffs"` bypasses a separate platform scan (differ-selection at plugin init time, not snapshot-selection); `platform` still governs which `unpack_config` entry handles each pull.

containerd v2.0.x predates `check_platform_supported` and always uses `platforms.Only` (exact OS and architecture match) -- `"linux"` resolves as `linux/<host-arch>` and cross-architecture pulls fail because no entry matches.  The workaround is per-architecture entries, each with `differ = "tardiffs"`:

```toml
[[plugins."io.containerd.transfer.v1.local".unpack_config]]
  snapshotter = "tarfs"
  differ      = "tardiffs"
  platform    = "linux/amd64"

[[plugins."io.containerd.transfer.v1.local".unpack_config]]
  snapshotter = "tarfs"
  differ      = "tardiffs"
  platform    = "linux/arm64"
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

## Future improvements

### Skip decompression entirely at pull time

The tardiffs proxy differ currently decompresses each layer blob in full (streaming, no disk writes) to compute the diffID.  This is necessary because containerd's unpack pipeline knows the expected diffID -- it's in the image config, already verified through the manifest chain -- but never passes it to the differ's `Apply` call.  If it did, tardiffs could return it directly and skip decompression entirely.

The change is small: add an `ExpectedLayerDiffID digest.Digest` field to `diff.ApplyConfig` in `core/diff/diff.go`, thread `diffIDs[i]` from the unpacker (which has it in scope at the `a.Apply()` call site in `core/unpack/unpacker.go`) into the `ApplyOpts`, and encode it in `diff.v1.ApplyRequest` (via the existing `payloads` map or a new first-class field).  With that in place, tardiffs can short-circuit to verifying the blob exists and returning the provided diffID -- zero decompression on any pull.

### Use a single name for both proxy plugins

The plugin registry uses `(type, id)` pairs as the uniqueness key, so `SnapshotPlugin/"tarfs"` and `DiffPlugin/"tardiffs"` coexist without conflict -- native plugins do exactly this (the `erofs` snapshotter and differ share `id = "erofs"`, registered in separate `init()` calls).  For proxy plugins the config struct only supports one `type` per `[proxy_plugins.<name>]` stanza, and TOML forbids duplicate keys, forcing the separate `tarfs` and `tardiffs` entries.

Changing `ProxyPlugin.Type string` to `Types []string` in `cmd/containerd/server/config/config.go` and iterating in `LoadPlugins()` would collapse this to a single stanza:

```toml
[proxy_plugins.tarfs]
  types   = ["snapshot", "diff"]
  address = "/run/containerd-snapshotter-tarfs/snapshotter.sock"

[[plugins."io.containerd.transfer.v1.local".unpack_config]]
  snapshotter = "tarfs"
  differ      = "tarfs"
```

### Fix `WithTempMount` cleanup in upstream containerd

`core/mount/temp.go`'s `WithTempMount` uses `os.Remove` to clean up the temp directory after the callback returns.  `os.Remove` fails silently with `ENOTEMPTY` on a non-empty directory, so any caller that invokes `WithTempMount` with zero mounts and a callback that writes files permanently leaks the extracted content under `/var/lib/containerd/tmpmounts/`.  The tarfs snapshotter returns empty mounts for extraction-keyed snapshots; with tardiffs configured containerd never reaches `WithTempMount` for those snapshots, but the underlying bug affects other callers and should be fixed upstream:

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
