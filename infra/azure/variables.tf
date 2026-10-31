variable "name" {
  description = "Prefix for resource names."
  type        = string
  default     = "artifactspoc"
}

variable "location" {
  description = "Azure region."
  type        = string
  default     = "westeurope"
}

variable "storage_account_name" {
  description = "Globally unique storage account name."
  type        = string
}

variable "content_domain" {
  description = "Domain under which artifacts are served, for example usercontent.example.com."
  type        = string
}

variable "app_origin" {
  description = "Origin allowed to embed artifacts, for example https://app.example.com."
  type        = string
}

variable "dns_zone_id" {
  description = "ID of the Azure DNS zone that holds content_domain."
  type        = string
}

variable "key_vault_id" {
  description = "ID of the Key Vault that holds the wildcard certificate. The vault must use Azure RBAC."
  type        = string
}

variable "wildcard_certificate_id" {
  description = "Versionless Key Vault certificate ID for *.content_domain."
  type        = string
}

variable "safety_net_days" {
  description = "Deletes blobs not modified for this long. Must stay above the longest retention the API allows (3650 days)."
  type        = number
  default     = 3700

  validation {
    condition     = var.safety_net_days > 3650
    error_message = "safety_net_days must exceed the API's maximum retention of 3650 days, or live artifacts could be deleted."
  }
}
