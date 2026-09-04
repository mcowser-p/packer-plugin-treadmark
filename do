#!/usr/bin/env bash
# Task runner (same spirit as holy-qcow's ./do). The GNUmakefile drives CI;
# this wraps the identical commands for hosts without make.
set -euo pipefail
cd "$(dirname "$0")"

sdk_version() { go list -m github.com/hashicorp/packer-plugin-sdk | cut -d' ' -f2; }

cmd=${1:-help}
shift || true

case "$cmd" in
  build) # compile the plugin binary
    go build -o packer-plugin-treadmark .
    ;;
  dev) # build + install into the local packer plugin dir
    "$0" build
    packer plugins install --path packer-plugin-treadmark github.com/mcowser-p/treadmark
    ;;
  test) # vet + unit tests (race)
    go vet ./...
    go test -race -count 1 ./... -timeout 3m
    ;;
  generate) # regenerate hcl2spec + docs partials (commit the output)
    go install "github.com/hashicorp/packer-plugin-sdk/cmd/packer-sdc@$(sdk_version)"
    go generate ./...
    ;;
  plugin-check) # packer-sdc conformance check of the built binary
    go install "github.com/hashicorp/packer-plugin-sdk/cmd/packer-sdc@$(sdk_version)"
    "$0" build
    packer-sdc plugin-check packer-plugin-treadmark
    ;;
  testacc) # acceptance tests (docker builder; needs ./do dev first)
    PACKER_ACC=1 go test -count 1 -v ./provisioner/... ./post-processor/... -timeout 120m
    ;;
  e2e-offline) # offline capture against a real qcow2: ./do e2e-offline IMG [TREADMARK_BIN]
    img=${1:?usage: ./do e2e-offline /path/to/image.qcow2 [/path/to/treadmark]}
    PACKER_ACC=1 TREADMARK_E2E_QCOW2=$img TREADMARK_E2E_BIN=${2:-} \
      go test -count 1 -v ./post-processor/treadmark -run TestOfflineE2E -timeout 60m
    ;;
  help|*)
    grep -E '^  [a-z-]+\)' "$0" | sed -e 's/)[[:space:]]*#/  —/' -e 's/^ */  /'
    ;;
esac
