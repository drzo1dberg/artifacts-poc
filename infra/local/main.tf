# The simulated storage account as Terraform, so the plan and apply flow can be tried without a cloud login.
# It runs next to the Compose stack on its own port and volume, and the two do not share data.

terraform {
  required_version = ">= 1.9"

  required_providers {
    docker = {
      source  = "kreuzwerker/docker"
      version = "~> 3.0"
    }
  }
}

provider "docker" {}

variable "port" {
  description = "Local port of the simulated storage account."
  type        = number
  default     = 9100
}

resource "docker_image" "blobstore" {
  name = "artifacts-poc-blobstore:terraform"

  build {
    context = "${path.module}/../../blobstore"
  }

  triggers = {
    source = filesha256("${path.module}/../../blobstore/main.go")
  }
}

resource "docker_volume" "storage_account" {
  name = "artifacts-poc-storage-account"
}

resource "docker_container" "storage_account" {
  name  = "artifacts-poc-storage-account"
  image = docker_image.blobstore.image_id

  ports {
    internal = 9000
    external = var.port
    ip       = "127.0.0.1"
  }

  volumes {
    volume_name    = docker_volume.storage_account.name
    container_path = "/data"
  }
}

output "blob_url" {
  description = "Base URL of the simulated storage account."
  value       = "http://127.0.0.1:${var.port}"
}
