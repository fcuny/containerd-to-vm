package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	containerdSock   = "/run/containerd/containerd.sock"
	defaultNamespace = "c2vm"
)

var (
	platform = platforms.Only(ocispec.Platform{
		OS:           "linux",
		Architecture: "amd64",
	})
)

func main() {
	var (
		containerName = flag.String("container", "", "Name of the container")
	)

	flag.Parse()

	if *containerName == "" {
		log.Fatal("a container is required")
	}

	client, err := containerd.New(containerdSock)
	if err != nil {
		log.Fatalf("failed to create a client for containerd: %v", err)
	}
	defer client.Close()

	ctx := namespaces.WithNamespace(context.Background(), defaultNamespace)
	ctx, done, err := client.WithLease(ctx)
	if err != nil {
		log.Fatalf("failed to get a lease: %v", err)
	}
	defer done(ctx)

	image, err := client.Pull(ctx, *containerName, containerd.WithPlatformMatcher(platform))
	if err != nil {
		log.Fatalf("failed to pull the container %s: %v\n", *containerName, err)
	}

	imageSize, err := image.Usage(ctx, containerd.WithUsageManifestLimit(1))
	if err != nil {
		log.Fatalf("failed to get the size of the image: %v", err)
	}

	fmt.Printf("pulled %s (%d bytes)\n", image.Name(), imageSize)
}
