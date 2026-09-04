# Changelog

## Unreleased

- `provisioner "treadmark"`: in-guest baseline capture for Linux (deb/rpm/
  standalone-binary install) and Windows (MSI, `all init` for files +
  registry), with verification scan, report generation, provenance capture
  (`baseline info --json`), and bundle download to the host.
- `post-processor "treadmark"`: collect mode (wraps the provisioner bundle +
  input artifact into one artifact, so `manifest` records both) and offline
  mode (guestmount + `files init --root`, image untouched); optional `s3 {}`
  upload blocks (default AWS credential chain, endpoint override +
  path-style for S3-compatible stores; buckets are never created).
- Bundle format: baseline.db, baseline-info.json, optional reports,
  registry-scan.json (Windows), metadata.json sidecar, SHA256SUMS.
