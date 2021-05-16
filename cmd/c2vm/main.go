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
	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/google/renameio"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	containerdSock   = "/run/containerd/containerd.sock"
	defaultNamespace = "c2vm"
)

var (
	firecrackerSock = filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "firecracker.sock")
	platform        = platforms.Only(ocispec.Platform{
		OS:           "linux",
		Architecture: "amd64",
	})
)

func main() {
	var (
		containerName     = flag.String("container", "", "Name of the container")
		outFile           = flag.String("out", "container.img", "Path to store the image")
		kernel            = flag.String("kernel", "", "Path to the linux kernel image")
		firecrackerBinary = flag.String("firecracker-binary", "", "Path to the firecracker binary")
		metricsFifo       = flag.String("metrics-fifo", "", "FIFO to the firecracker metrics")
	)

	flag.Parse()

	if *containerName == "" {
		log.Fatal("a container is required")
	}

	if *kernel == "" {
		log.Fatalf("a linux kernel is required")
	}

	if *firecrackerBinary == "" {
		log.Fatalf("the path to the firecracker binary is required")
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

	bootVM(ctx, *outFile, *kernel, *firecrackerBinary, *metricsFifo)
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
	if err != nil {
		return err
	}
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

func bootVM(ctx context.Context, rawImage, kernel, firecrackerBinary, metricsFifo string) {
	vmmCtx, vmmCancel := context.WithCancel(ctx)
	defer vmmCancel()

	devices := make([]models.Drive, 1)
	devices[0] = models.Drive{
		DriveID:      firecracker.String("1"),
		PathOnHost:   &rawImage,
		IsRootDevice: firecracker.Bool(true),
		IsReadOnly:   firecracker.Bool(false),
	}
	fcCfg := firecracker.Config{
		LogLevel:        "debug",
		SocketPath:      firecrackerSock,
		KernelImagePath: kernel,
		KernelArgs:      "console=ttyS0 reboot=k panic=1 acpi=off pci=off i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd init=/init.sh random.trust_cpu=on",
		Drives:          devices,
		MetricsFifo:     metricsFifo,
		MachineCfg: models.MachineConfiguration{
			VcpuCount:   firecracker.Int64(1),
			CPUTemplate: models.CPUTemplate("C3"),
			HtEnabled:   firecracker.Bool(true),
			MemSizeMib:  firecracker.Int64(512),
		},
		NetworkInterfaces: []firecracker.NetworkInterface{
			{
				CNIConfiguration: &firecracker.CNIConfiguration{
					NetworkName: "c2vm",
					IfName:      "eth0",
				},
			},
		},
	}

	machineOpts := []firecracker.Opt{}

	command := firecracker.VMCommandBuilder{}.
		WithBin(firecrackerBinary).
		WithSocketPath(fcCfg.SocketPath).
		WithStdin(os.Stdin).
		WithStdout(os.Stdout).
		WithStderr(os.Stderr).
		Build(ctx)
	machineOpts = append(machineOpts, firecracker.WithProcessRunner(command))
	m, err := firecracker.NewMachine(vmmCtx, fcCfg, machineOpts...)
	if err != nil {
		fmt.Printf("failed to start the vm: %+v\n", err)
		os.Exit(1)
	}

	if err := m.Start(vmmCtx); err != nil {
		fmt.Printf("failed to start the vm: %+v\n", err)
		os.Exit(1)
	}
	defer m.StopVMM()

	if err := m.Wait(vmmCtx); err != nil {
		fmt.Printf("failed to start the vm: %+v\n", err)
		os.Exit(1)
	}
	log.Print("Machine was started")
}
