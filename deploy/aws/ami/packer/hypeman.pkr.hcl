packer {
  required_plugins {
    amazon = {
      version = ">= 1.3.0"
      source  = "github.com/hashicorp/amazon"
    }
  }
}

variable "region" {
  type    = string
  default = "us-east-1"
}

variable "instance_type" {
  type    = string
  default = "c8i.large"
}

variable "hypeman_version" {
  type    = string
  default = "latest"
}

source "amazon-ebs" "hypeman" {
  region          = var.region
  instance_type   = var.instance_type
  ssh_username    = "ubuntu"
  ami_name        = "hypeman-{{timestamp}}"
  ami_description = "Hypeman API host image"

  source_ami_filter {
    filters = {
      name                = "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"
      root-device-type    = "ebs"
      virtualization-type = "hvm"
    }
    owners      = ["099720109477"]
    most_recent = true
  }

  launch_block_device_mappings {
    device_name           = "/dev/sda1"
    volume_size           = 100
    volume_type           = "gp3"
    encrypted             = true
    delete_on_termination = true
  }
}

build {
  sources = ["source.amazon-ebs.hypeman"]

  provisioner "shell" {
    inline = [
      "set -euxo pipefail",
      "sudo apt-get update",
      "sudo DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl docker.io e2fsprogs erofs-utils iproute2 iptables jq openssl qemu-system-x86 qemu-utils tar",
      "sudo systemctl enable docker",
      "if [ '${var.hypeman_version}' = 'latest' ]; then curl -fsSL https://raw.githubusercontent.com/kernel/hypeman/main/scripts/install.sh | sudo bash; else curl -fsSL https://raw.githubusercontent.com/kernel/hypeman/main/scripts/install.sh | sudo VERSION='${var.hypeman_version}' bash; fi",
      "sudo systemctl stop hypeman",
      "sudo rm -f /root/.config/hypeman/cli.yaml",
      "sudo cloud-init clean --logs",
    ]
  }
}
