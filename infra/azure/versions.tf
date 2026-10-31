terraform {
  required_version = ">= 1.9"

  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 4.81"
    }
    azapi = {
      source  = "Azure/azapi"
      version = "~> 2.0"
    }
  }
}

provider "azurerm" {
  features {}

  # The account has Shared Key turned off, so the provider must use Entra ID for storage calls too.
  storage_use_azuread = true
}

provider "azapi" {}
