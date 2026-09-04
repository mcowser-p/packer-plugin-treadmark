# Offline capture — baseline an already-built qcow2 without touching it.
# The null builder contributes nothing; the post-processor does the work on
# the build host (guestmount + `treadmark files init --root`). In a real
# pipeline you'd instead chain this post-processor directly after the qemu
# builder and drop image_path (it defaults to the input artifact's qcow2).

packer {
  required_plugins {
    # The null builder ships inside Packer core — no plugin entry needed.
    treadmark = {
      version = ">= 0.1.0"
      source  = "github.com/mcowser-p/treadmark"
    }
  }
}

variable "image" {
  type        = string
  description = "Path to the qcow2 to baseline."
}

source "null" "image" {
  communicator = "none"
}

build {
  sources = ["source.null.image"]

  post-processor "treadmark" {
    mode          = "offline"
    image_path    = var.image
    config_source = "${path.root}/../config/golden-linux.yaml"
    output_dir    = "${path.root}/output"
    # Host prerequisites: guestmount (libguestfs) and treadmark on PATH.
    # SUPERMIN_KERNEL/SUPERMIN_MODULES are auto-pinned to the running
    # kernel; override via extra_env if needed.
    report_formats = ["json"]
  }
}
