.PHONY: build
build:
	go build -o c2vm cmd/c2vm/main.go

.PHONY: firecracker-binary
firecracker-binary:
	mkdir -p hack/firecracker
	curl -L -o hack/firecracker/firecracker.tgz https://github.com/firecracker-microvm/firecracker/releases/download/v0.24.3/firecracker-v0.24.3-x86_64.tgz
	tar xvzf hack/firecracker/firecracker.tgz -C hack/firecracker
