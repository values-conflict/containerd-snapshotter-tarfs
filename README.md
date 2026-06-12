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
- `--state-dir` -- directory for snapshotter metadata and FUSE mount points (default: `/var/lib/containerd-snapshotter-tarfs`)
- `--containerd-socket` -- containerd gRPC socket, for content store access (default: `/run/containerd/containerd.sock`)

## Configuring containerd

Add the following to your `config.toml` (typically `/etc/containerd/config.toml`):

```toml
[proxy_plugins]
  [proxy_plugins.tarfs]
    type = "snapshot"
    address = "/run/containerd-snapshotter-tarfs/snapshotter.sock"

[[plugins."io.containerd.transfer.v1.local".unpack_config]]
  snapshotter = "tarfs"
  differ = ""
```

The `unpack_config` entry is required so that the transfer service sets `containerd.io/snapshot/diff-id` labels during image pulls -- without it, layer blobs can't be resolved back to their content store entries.

After updating the config, start `containerd-snapshotter-tarfs` before (or alongside) `containerd`, then pull and run images using `--snapshotter tarfs`:

```console
$ ctr images pull --snapshotter tarfs docker.io/library/hello-world:latest
$ ctr images mount --snapshotter tarfs docker.io/library/hello-world:latest /mnt/hello-world
```

## Notes

FUSE mounts require `CAP_SYS_ADMIN` (or a user namespace with `--map-root-user`).  In a dev environment without root, wrap all processes in a shared namespace:

```console
$ unshare --user --map-root-user --mount -- bash
```

All processes that need to share FUSE mounts (`containerd`, `containerd-snapshotter-tarfs`, `ctr`) must run inside the _same_ namespace -- each separate `unshare` call creates a distinct mount namespace.
