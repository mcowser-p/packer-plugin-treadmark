# treadmark plugin documentation

Components:

- [`provisioner "treadmark"`](provisioners/treadmark.mdx) — in-guest
  baseline capture (Linux + Windows).
- [`post-processor "treadmark"`](post-processors/treadmark.mdx) — collect
  the bundle into the build artifact, offline capture from the built image,
  optional S3 upload.

Attribute tables under `docs-partials/` are generated from the Go config
structs by `packer-sdc struct-markdown` (`./do generate`); edit the struct
doc comments, not the partials.
