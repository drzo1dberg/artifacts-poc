output "storage_blob_endpoint" {
  description = "Blob endpoint the API talks to."
  value       = azurerm_storage_account.this.primary_blob_endpoint
}

output "api_identity_client_id" {
  description = "Client ID of the managed identity the API runs as."
  value       = azurerm_user_assigned_identity.api.client_id
}

output "frontdoor_endpoint_hostname" {
  description = "Front Door hostname. Point a wildcard CNAME for content_domain at it."
  value       = azurerm_cdn_frontdoor_endpoint.content.host_name
}

output "custom_domain_validation_token" {
  description = "Token for the _dnsauth TXT record of content_domain."
  value       = azurerm_cdn_frontdoor_custom_domain.wildcard.validation_token
}
