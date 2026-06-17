// containerd-snapshotter-tarfs is a containerd proxy snapshotter that mounts OCI image layers directly from the content store via FUSE, with no extraction step.  Layers are served live from their tar blobs using tarfs (for plain tarballs) or gsip+tarfs (for gzip-compressed blobs).
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	"github.com/containerd/containerd/v2/contrib/snapshotservice"
	"github.com/containerd/containerd/v2/core/content/proxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		socketPath       = flag.String("socket", "/run/containerd-snapshotter-tarfs/snapshotter.sock", "unix socket path for the snapshotter gRPC server")
		stateDir         = flag.String("state-dir", "/run/containerd-snapshotter-tarfs", "directory for FUSE mount points and overlay upper/work dirs (runtime state; not preserved across restarts)")
		containerdSocket = flag.String("containerd-socket", "/run/containerd/containerd.sock", "containerd gRPC socket (for content store access)")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// connect to containerd to use its content store via gRPC -- this avoids
	// direct BoltDB access and the associated exclusive-lock contention
	conn, err := grpc.NewClient("unix://"+*containerdSocket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("connecting to containerd at %q: %w", *containerdSocket, err)
	}
	defer conn.Close()

	cs := proxy.NewContentStore(conn)

	sn, err := NewSnapshotter(ctx, *stateDir, cs)
	if err != nil {
		return fmt.Errorf("creating snapshotter at %q: %w", *stateDir, err)
	}
	defer sn.Close()

	rpc := grpc.NewServer()
	snapshotsapi.RegisterSnapshotsServer(rpc, snapshotservice.FromSnapshotter(sn))

	if err := os.MkdirAll(filepath.Dir(*socketPath), 0o700); err != nil {
		return fmt.Errorf("creating socket directory: %w", err)
	}
	_ = os.Remove(*socketPath)

	l, err := net.Listen("unix", *socketPath)
	if err != nil {
		return fmt.Errorf("listening on %q: %w", *socketPath, err)
	}
	defer l.Close()

	fmt.Printf("tarfs snapshotter listening on %s\n", *socketPath)

	errCh := make(chan error, 1)
	go func() {
		errCh <- rpc.Serve(l)
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("gRPC server: %w", err)
	case <-ctx.Done():
		rpc.GracefulStop()
		return nil
	}
}
