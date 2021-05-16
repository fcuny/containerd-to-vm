FC_VERSION?=0.24.3-x86_64
FC_BINARY?=hack/firecracker/firecracker-v$(FC_VERSION)

FCNET_CONFIG?=/etc/cni/conf.d/50-c2vm.conflist
CNI_BIN_ROOT?=/opt/cni/bin
CNI_TAP_PLUGIN?=$(CNI_BIN_ROOT)/tc-redirect-tap

.PHONY: build
build:
	@go build -o c2vm cmd/c2vm/main.go

$(FC_BINARY):
	@mkdir -p hack/firecracker
	@curl -L -o hack/firecracker/firecracker.tgz -s https://github.com/firecracker-microvm/firecracker/releases/download/v0.24.3/firecracker-v$(FC_VERSION).tgz
	@tar xvzf hack/firecracker/firecracker.tgz -C hack/firecracker

$(FCNET_CONFIG):
	@sudo mkdir -p $(dir $(FCNET_CONFIG))
	@sudo cp hack/cni/50-c2vm.conflist $(FCNET_CONFIG)

$(CNI_TAP_PLUGIN):
	@go install github.com/awslabs/tc-redirect-tap/cmd/tc-redirect-tap@latest
	@sudo cp ${GOPATH}/bin/tc-redirect-tap $(CNI_BIN_ROOT)

.PHONY: all
all: build $(FC_BINARY) $(FCNET_CONFIG) $(CNI_TAP_PLUGIN)
