# In-guest capture, Windows — files + registry (`all init`) over SSH.
# Shown with the null builder pointed at an existing machine so the config
# validates standalone; in a real pipeline the provisioner drops into your
# qemu/hyperv Windows template after the last cleanup step and before any
# sysprep shutdown_command (nothing can capture after sysprep runs).

packer {
  required_plugins {
    # The null builder ships inside Packer core — no plugin entry needed.
    treadmark = {
      version = ">= 0.1.0"
      source  = "github.com/mcowser-p/treadmark"
    }
  }
}

variable "host" {
  type = string
}

variable "user" {
  type    = string
  default = "Administrator"
}

variable "ssh_private_key_file" {
  type = string
}

source "null" "win" {
  ssh_host             = var.host
  ssh_username         = var.user
  ssh_private_key_file = var.ssh_private_key_file
}

build {
  sources = ["source.null.win"]

  provisioner "treadmark" {
    os            = "windows"
    scope         = "all" # files + registry into one baseline.db
    config_source = "${path.root}/../config/golden-windows.yaml"
    # MSI install can take a while on first boot; `all init` over Program
    # Files longer still.
    init_timeout   = "45m"
    report_formats = ["json"]
    output_dir     = "${path.root}/output"
  }

  post-processors {
    post-processor "treadmark" {
      output_dir = "${path.root}/output"
    }
    post-processor "manifest" {
      output     = "${path.root}/output/manifest.json"
      strip_path = true
    }
  }
}
