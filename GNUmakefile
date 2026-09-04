NAME=treadmark
BINARY=packer-plugin-$(NAME)
PLUGIN_FQN=$(shell grep -E '^module' go.mod | sed -E 's/^module *//')
HASHICORP_PACKER_PLUGIN_SDK_VERSION?=$(shell go list -m github.com/hashicorp/packer-plugin-sdk | cut -d " " -f2)
COUNT?=1
TEST?=$(shell go list ./...)

.PHONY: build dev test testacc install-packer-sdc generate plugin-check e2e-offline

build:
	@go build -o $(BINARY)

# Build with a dev prerelease and install into the local Packer plugin dir so
# templates can consume `source = "github.com/mcowser-p/treadmark"`.
dev: build
	@packer plugins install --path $(BINARY) github.com/mcowser-p/treadmark

test:
	@go vet ./...
	@go test -race -count $(COUNT) $(TEST) -timeout=3m

install-packer-sdc:
	@go install github.com/hashicorp/packer-plugin-sdk/cmd/packer-sdc@$(HASHICORP_PACKER_PLUGIN_SDK_VERSION)

generate: install-packer-sdc
	@go generate ./...

plugin-check: install-packer-sdc build
	@packer-sdc plugin-check $(BINARY)

# Acceptance tests drive real `packer build`s (docker builder); they only run
# with PACKER_ACC=1 and need `make dev` + `packer init` beforehand.
testacc:
	@PACKER_ACC=1 go test -count 1 -v ./provisioner/... ./post-processor/... -timeout=120m

# Local-only: offline capture against an existing qcow2 (KVM/libguestfs host).
# Usage: make e2e-offline QCOW2=/path/to/image.qcow2 [TREADMARK_BIN=/path/to/treadmark]
e2e-offline:
	@test -n "$(QCOW2)" || (echo "usage: make e2e-offline QCOW2=/path/to/image.qcow2" && exit 1)
	@PACKER_ACC=1 TREADMARK_E2E_QCOW2=$(QCOW2) TREADMARK_E2E_BIN=$(TREADMARK_BIN) \
		go test -count 1 -v ./post-processor/treadmark -run TestOfflineE2E -timeout=60m
