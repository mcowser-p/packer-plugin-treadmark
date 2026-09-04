# Acceptance template: in-guest capture inside an ubuntu container (docker
# builder runs as root — disable_sudo). Driven by provisioner_acc_test.go.

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

variable "output_dir" {
  type = string
}

source "docker" "ubuntu" {
  image   = "ubuntu:24.04"
  discard = true
}

build {
  name    = "treadmark-acc"
  sources = ["source.docker.ubuntu"]

  provisioner "treadmark" {
    install_method = "deb"
    config_source  = "${path.root}/config-min.yaml"
    disable_sudo   = true
    report_formats = ["json"]
    output_dir     = var.output_dir
    metadata = {
      acceptance = "docker-linux"
    }
  }

  post-processors {
    post-processor "treadmark" {
      output_dir = var.output_dir
    }
    post-processor "manifest" {
      output     = "${var.output_dir}/manifest.json"
      strip_path = true
    }
  }
}
