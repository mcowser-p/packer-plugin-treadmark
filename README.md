# Packer Plugin: treadmark

Capture a [treadmark](https://github.com/mcowser-p/treadmark) file-integrity
baseline as part of a Packer golden-image build, and ship it as a build
artifact and/or to S3.

treadmark's golden-baseline workflow starts with "capture a baseline on a
freshly-provisioned, hardened, known-clean machine" — which is exactly what a
Packer build is at its final provisioning step. This plugin automates that:
every image build produces (a) a baseline baked into the image so clones can
self-verify, and/or (b) an evidence bundle on the build host — `baseline.db`,
provenance, drift reports, `metadata.json`, `SHA256SUMS` — ready for the
`manifest` post-processor, a GitHub release, or an S3 bucket.

## Installation

```hcl
packer {
  required_plugins {
    treadmark = {
      version = ">= 0.1.0"
      source  = "github.com/mcowser-p/treadmark"
    }
  }
}
```

Then `packer init .`. For local development builds: `./do dev` (or
`make dev`) compiles and installs the plugin under the same source address.

## Components

### `provisioner "treadmark"` — in-guest capture

Installs treadmark inside the guest (deb/rpm/standalone binary on Linux, MSI
on Windows — or `install_method = "none"` if it's already there), uploads
your config, runs `files init` (`all init` on Windows: files + registry),
verifies with a scan, records provenance, and downloads the bundle to
`output_dir` on the host. The baseline stays in the image at
`/var/lib/treadmark/baseline.db` (clones can `treadmark files scan`
immediately) unless `remove_from_image = true`.

```hcl
provisioner "treadmark" {
  treadmark_version = "0.11.0"
  config_source     = "path/to/golden-linux.yaml"
  report_formats    = ["json"]
  output_dir        = "build/output/${local.name}/treadmark"
  metadata          = { distro = "almalinux", build_number = var.build_number }
}
```

Notes that matter on hardened images:

- Every command runs the installed binary by full path with `sudo`; nothing
  is ever executed from the staging dir — immune to noexec `/tmp`/`/var/tmp`
  (CIS remediation mounts those noexec).
- On Windows, `treadmark.exe` is always invoked by full path — the MSI's
  PATH entry is invisible to the already-open SSH session.
- Scan exit codes are treadmark API: 0 clean, 1 drift, 2 error. The
  verification scan expects 0; `on_drift = "warn"` downgrades exit 1 (a
  drift right after init means the config monitors something volatile —
  tune excludes). Exit 2 always fails the build.

### `post-processor "treadmark"` — artifact, offline capture, S3

Two modes:

- **`mode = "collect"`** (default) — wraps the provisioner's bundle plus the
  input artifact into one artifact. Chain it *before* `manifest` and
  manifest.json records the image and the baseline files together. The input
  artifact is always force-kept (same contract as `manifest`) — the qcow2
  can never be silently discarded by this step.
- **`mode = "offline"`** — no guest involvement: guestmounts the built image
  read-only on the build host and runs the host's treadmark with `--root`,
  writing the baseline straight to `output_dir`. The image ships untouched,
  and the capture happens *after* the shutdown/sysprep step — it baselines
  the exact bits that ship. Linux images only; needs `guestmount`
  (libguestfs) and `treadmark` on the build host. Baselines store logical
  paths, so an offline DB is comparable against live hosts.

Either mode takes optional `s3 { }` blocks:

```hcl
post-processor "treadmark" {
  output_dir = "build/output/${local.name}/treadmark"
  s3 {
    bucket = "golden-evidence-123456789012-us-east-1"
    prefix = "treadmark/${local.name}/"
    # endpoint + force_path_style work for MinIO/any S3-compatible store:
    # endpoint         = "https://minio.lab:9000"
    # force_path_style = true
  }
}
```

Credentials come from the default AWS chain. The bucket must already exist
(HeadBucket check) — this plugin never creates buckets. The uploaded
`metadata.json` records its own object keys, and `SHA256SUMS` is finalized
before upload so it covers exactly what lands in the bucket.

Full attribute reference: [`docs/`](docs/) (generated from the config
structs; see also `example/`).

## The bundle

```
treadmark-output/<build>/
├── baseline.db           # the baseline (sha256 cross-checked after download)
├── baseline-info.json    # `treadmark baseline info --json` provenance
├── init-scan.json        # verification-scan report(s), per report_formats
├── registry-scan.json    # Windows: registry half as JSON
├── metadata.json         # build name/uuid, versions, scope, sha256, s3 keys
└── SHA256SUMS            # chain-of-custody over the bundle
```

## holy-qcow integration sketch

In a hardened qemu template (e.g. alma10): provisioner between
`harden-cis.sh` and `sysprep.sh` (captures the hardened state), collect
post-processor before `manifest`:

```hcl
provisioner "treadmark" {
  treadmark_version = "0.11.0"
  config_source     = "${path.root}/../common/treadmark-golden-linux.yaml"
  report_formats    = ["json"]
  output_dir        = "${path.root}/../../build/output/${local.name}/treadmark"
  metadata          = { distro = "almalinux", build_number = var.build_number }
}
```

The exclude list must cover what changes *after* capture (sysprep, first
boot): `example/config/golden-linux.yaml` handles machine-id, host keys,
cloud-init state, `/home/packer`, and the cloud-init sudoers drop-in.
Alternatively skip the provisioner and run `mode = "offline"` after the
build — forensically stronger (baselines the sealed image), at the cost of
libguestfs on the build host and no in-image baseline. Windows templates:
the provisioner must run before the sysprep `shutdown_command` — nothing can
capture after it.

## Development

No Go on the machine? `~/.local/go` + this repo is all the CI needs. Common
tasks (`./do <cmd>`, or `make <cmd>` where make exists):

```
./do build         # compile
./do dev           # compile + install into the local packer plugin dir
./do test          # vet + unit tests (-race)
./do generate      # regenerate hcl2spec + docs partials (commit the output)
./do plugin-check  # packer-sdc conformance check
./do testacc       # acceptance tests (real packer build, docker builder)
./do e2e-offline /path/to/image.qcow2 [treadmark-bin]
```

Unit tests cover config validation, the exact guest command lines (golden
strings over a scripted mock communicator), release download/verify, S3 key
layout/SSE propagation, and bundle assembly. CI additionally runs a real
docker-builder build and a MinIO round-trip.

## Releasing

Conventional Commits; manual tags while 0.x. One-time setup: `GPG_PRIVATE_KEY`
and `GPG_PASSPHRASE` repo secrets (`packer init` requires a signed
SHA256SUMS). Then:

```
git tag v0.1.0 && git push origin v0.1.0
```

The release workflow builds per-platform zips (`x5.0` API suffix), the
SHA256SUMS, and its `.sig`, and publishes the GitHub release `packer init`
consumes.

## License

Apache-2.0, same as treadmark.
