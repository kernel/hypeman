# Builds a macOS image for hypeman on a stacked Tart VM. Clone the base with
# `tart clone --stacked` first and pass that VM as vm_base_name: the build writes
# only to the clone's overlay, which import-tart publishes over the base.
# See docs/macos-experimental.md.
packer {
  required_plugins {
    tart = {
      version = "= 1.21.0"
      source  = "github.com/cirruslabs/tart"
    }
  }
}

variable "vm_base_name" {
  type        = string
  description = "Stopped Tart VM to build on, made with tart clone --stacked."
}

variable "vm_name" {
  type        = string
  description = "Tart VM the build writes, which import-tart publishes."
}

variable "ssh_username" {
  type = string
}

variable "ssh_password" {
  type      = string
  sensitive = true
}

variable "extra_scripts" {
  type        = list(string)
  default     = []
  description = "Scripts run as root, in order, after Chrome is installed."
}

locals {
  sudo = "echo '${var.ssh_password}' | sudo -S -p '' sh -eux '{{ .Path }}'"
}

source "tart-cli" "macos" {
  vm_base_name = var.vm_base_name
  vm_name      = var.vm_name
  headless     = true
  # The default, "delete", removes the recovery partition and grows APFS, which
  # rewrites the whole disk instead of the build's changes.
  recovery_partition = "keep"
  ssh_username       = var.ssh_username
  ssh_password       = var.ssh_password
  ssh_timeout        = "300s"
}

build {
  sources = ["source.tart-cli.macos"]

  provisioner "shell" {
    execute_command = local.sudo
    scripts         = concat(["${path.root}/install-chrome.sh"], var.extra_scripts)
  }

  # hypeman supplies its guest agent on a read-only disk; the image only carries
  # the launcher that runs it.
  provisioner "shell" {
    execute_command = local.sudo
    script          = "${path.root}/../../lib/system/guest_agent/macos/install-launcher.sh"
  }

  provisioner "shell" {
    script = "${path.root}/smoke-test.sh"
  }

  # Every image gets its own SSH host keys; Tart keeps its source's. The guest
  # shuts itself down: tart stop is a hard power-off.
  provisioner "shell" {
    execute_command = local.sudo
    inline = [
      "rm -f /etc/ssh/ssh_host_*",
      "ssh-keygen -A",
      "shutdown -h now",
    ]
    expect_disconnect = true
    # Without this, Packer retries removing its script over the dead connection
    # for five minutes.
    skip_clean = true
  }
}
