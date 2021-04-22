package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/platforms"
	"github.com/docker/docker/pkg/archive"
	"github.com/google/renameio"
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
		outFile       = flag.String("out", "container.img", "firecracker output to create")
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

	log.Printf("pulled %s (%d bytes)\n", image.Name(), imageSize)

	mntDir, err := ioutil.TempDir("", "c2vm")
	if err != nil {
		log.Fatalf("Failed to create mount temp dir: %v\n", err)
	}

	if err := createLoopDevice(*outFile, mntDir); err != nil {
		log.Fatalf("%v\n", err)
	}

	if err := extract(ctx, client, image, mntDir); err != nil {
		log.Fatalf("failed to extract the container: %v\n", err)
	}

	if err = initScript(ctx, client, image, mntDir); err != nil {
		log.Fatalf("failed to create init script: %s\n", err)
	}

	if err = extraFiles(mntDir); err != nil {
		log.Fatalf("failed to add extra files to the image: %v\n", err)
	}

	if err := detachLoopDevice(mntDir); err != nil {
		log.Fatalf("failed to umount %s: %v\n", mntDir, err)
	}

	if err := resizeImage(*outFile); err != nil {
		log.Fatalf("failed to resize the image %s: %s\n", *outFile, err)
	}
}

func extract(ctx context.Context, client *containerd.Client, image containerd.Image, mntDir string) error {
	manifest, err := images.Manifest(ctx, client.ContentStore(), image.Target(), platform)
	if err != nil {
		log.Fatalf("failed to get the manifest: %v\n", err)
	}

	for _, desc := range manifest.Layers {
		log.Printf("extracting layer %s\n", desc.Digest.String())
		layer, err := client.ContentStore().ReaderAt(ctx, desc)
		if err != nil {
			return err
		}
		if err := archive.Untar(content.NewReader(layer), mntDir, &archive.TarOptions{NoLchown: true}); err != nil {
			return err
		}
	}

	return nil
}

func createLoopDevice(rawFile, mntDir string) error {
	f, err := renameio.TempFile("", rawFile)
	if err != nil {
		return err
	}
	defer f.Cleanup()

	command := exec.Command("fallocate", "-l", "2G", f.Name())
	if err := command.Run(); err != nil {
		return fmt.Errorf("fallocate error: %s", err)
	}

	command = exec.Command("mkfs.ext4", "-F", f.Name())
	if err := command.Run(); err != nil {
		return fmt.Errorf("mkfs.ext4 error: %s", err)
	}

	f.CloseAtomicallyReplace()

	command = exec.Command("mount", "-o", "loop", rawFile, mntDir)
	if err := command.Run(); err != nil {
		return fmt.Errorf("mount error: %s", err)
	}
	log.Printf("mounted %s on %s\n", rawFile, mntDir)
	return nil
}

func detachLoopDevice(mntDir string) error {
	log.Printf("umount %s\n", mntDir)
	command := exec.Command("umount", mntDir)
	return command.Run()
}

func resizeImage(rawFile string) error {
	// let's bring the image to a more reasonable size. We do this by
	// first running e2fsck on the image then we can resize the image.
	command := exec.Command("/usr/bin/e2fsck", "-p", "-f", rawFile)
	if err := command.Run(); err != nil {
		return fmt.Errorf("e2fsck error: %s", err)
	}

	command = exec.Command("resize2fs", "-M", rawFile)
	if err := command.Run(); err != nil {
		return fmt.Errorf("resize2fs error: %s", err)
	}
	return nil
}

func extraFiles(mntDir string) error {
	if err := writeToFile(filepath.Join(mntDir, "etc", "hosts"), "127.0.0.1\tlocalhost\n"); err != nil {
		return err
	}
	if err := writeToFile(filepath.Join(mntDir, "etc", "resolv.conf"), "nameserver 192.168.0.1\n"); err != nil {
		return err
	}
	return nil
}

func initScript(ctx context.Context, client *containerd.Client, image containerd.Image, mntDir string) error {
	config, err := images.Config(ctx, client.ContentStore(), image.Target(), platform)
	if err != nil {
		return err
	}

	configBlob, err := content.ReadBlob(ctx, client.ContentStore(), config)
	var imageSpec ocispec.Image
	json.Unmarshal(configBlob, &imageSpec)
	initCmd := strings.Join(imageSpec.Config.Cmd, " ")
	initEnvs := imageSpec.Config.Env

	initPath := filepath.Join(mntDir, "init.sh")
	f, err := renameio.TempFile("", initPath)
	if err != nil {
		return err
	}
	defer f.Cleanup()

	writer := bufio.NewWriter(f)
	fmt.Fprintf(writer, "#!/bin/sh\n")
	for _, env := range initEnvs {
		fmt.Fprintf(writer, "export %s\n", env)
	}
	fmt.Fprintf(writer, "%s\n", initCmd)
	writer.Flush()

	f.CloseAtomicallyReplace()

	mode := int(0755)
	os.Chmod(initPath, os.FileMode(mode))
	log.Printf("init script created")
	return nil
}

func writeToFile(filepath string, content string) error {
	if err := ioutil.WriteFile(filepath, []byte(content), 0644); err != nil {
		return fmt.Errorf("writeToFile %s: %v", filepath, err)
	}
	return nil
}
