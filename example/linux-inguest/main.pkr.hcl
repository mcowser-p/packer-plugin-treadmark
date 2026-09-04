# In-guest capture, Linux — smallest runnable example (docker builder).
# For a real golden-image pipeline (qemu builder, CIS hardening, manifest
# chain) see the "holy-qcow integration" section of the README.

packer {
  required_plugins {
    docker = {
      version = ">= 1.0.0"
      source  = "github.com/hashicorp/docker"
    }
    treadmark = {
      version = ">= 0.1.0"
      source  = "github.com/mcowser-p/treadmark"
    }
  }
}

source "docker" "ubuntu" {
  image   = "ubuntu:24.04"
  discard = true
}

build {
  sources = ["source.docker.ubuntu"]

  provisioner "treadmark" {
    # docker runs as root; on SSH-based builds drop disable_sudo.
    disable_sudo   = true
    config_source  = "${path.root}/../config/golden-linux.yaml"
    report_formats = ["json", "html"]
    output_dir     = "${path.root}/output"
    metadata = {
      distro = "ubuntu"
    }
  }

  post-processors {
    post-processor "treadmark" {
      output_dir = "${path.root}/output"
      # s3 {
      #   bucket = "my-evidence-bucket"
      #   prefix = "treadmark/linux-inguest/"
      # }
    }
    post-processor "manifest" {
      output     = "${path.root}/output/manifest.json"
      strip_path = true
    }
  }
}
