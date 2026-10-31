# The data plane of the POC on Azure: storage plus Front Door, with no SAS tokens and no account keys.
#
# Terraform owns what exists once per environment. The per-user containers are not here: the API creates
# them at runtime with its managed identity, because a Terraform run per login would be slow and would
# fight over the state lock.

locals {
  # Everything from the hostname after <artifact-id>--<user>. Its length drives the rewrite below.
  content_suffix = ".${var.content_domain}"

  # A UUID is always 36 characters and "--" adds 2, so the user starts at a fixed offset.
  # Front Door has no regex captures, which makes this fixed length the only reason the mapping works.
  rewrite_destination = "/u-{hostname:38:-${length(local.content_suffix)}}/{hostname:0:36}/"

  artifact_host_regex = "^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}--[a-z0-9]{1,20}${replace(local.content_suffix, ".", "\\.")}$"

  # Same policy as frontdoor/default.conf in the local stack.
  content_security_policy = "sandbox allow-scripts; default-src 'none'; script-src 'self' 'unsafe-inline' https://cdnjs.cloudflare.com; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; connect-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors ${var.app_origin}"
}

resource "azurerm_resource_group" "this" {
  name     = "rg-${var.name}"
  location = var.location
}

# --- Storage ---

resource "azurerm_storage_account" "this" {
  name                = var.storage_account_name
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location

  account_tier             = "Standard"
  account_replication_type = "LRS"
  min_tls_version          = "TLS1_2"

  # No account keys, so no key-signed SAS can exist. Every caller authenticates through Entra ID.
  shared_access_key_enabled       = false
  default_to_oauth_authentication = true
  allow_nested_items_to_be_public = false
}

# A safety net against orphaned blobs. Per-artifact retention stays in the API's job, because this policy
# does not know the database, and it works on blob age, so a shorter window could delete live artifacts.
resource "azurerm_storage_management_policy" "safety_net" {
  storage_account_id = azurerm_storage_account.this.id

  rule {
    name    = "delete-orphaned-blobs"
    enabled = true

    filters {
      blob_types = ["blockBlob"]
    }

    actions {
      base_blob {
        delete_after_days_since_modification_greater_than = var.safety_net_days
      }
    }
  }
}

# --- Identities: these replace SAS tokens ---

# The API writes blobs and creates containers. A container app would attach this identity.
resource "azurerm_user_assigned_identity" "api" {
  name                = "id-${var.name}-api"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
}

resource "azurerm_role_assignment" "api_storage" {
  scope                = azurerm_storage_account.this.id
  role_definition_name = "Storage Blob Data Contributor"
  principal_id         = azurerm_user_assigned_identity.api.principal_id
}

# Front Door only ever reads. Writing through the public hostname fails on this role alone.
resource "azurerm_user_assigned_identity" "frontdoor" {
  name                = "id-${var.name}-frontdoor"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
}

resource "azurerm_role_assignment" "frontdoor_storage" {
  scope                = azurerm_storage_account.this.id
  role_definition_name = "Storage Blob Data Reader"
  principal_id         = azurerm_user_assigned_identity.frontdoor.principal_id
}

resource "azurerm_role_assignment" "frontdoor_certificate" {
  scope                = var.key_vault_id
  role_definition_name = "Key Vault Secrets User"
  principal_id         = azurerm_user_assigned_identity.frontdoor.principal_id
}

# --- Front Door ---

# Standard is enough. Managed identity origin auth does not work with Private Link, so Premium buys nothing here.
resource "azurerm_cdn_frontdoor_profile" "this" {
  name                = "afd-${var.name}"
  resource_group_name = azurerm_resource_group.this.name
  sku_name            = "Standard_AzureFrontDoor"

  identity {
    type         = "UserAssigned"
    identity_ids = [azurerm_user_assigned_identity.frontdoor.id]
  }

  depends_on = [azurerm_role_assignment.frontdoor_certificate]
}

resource "azurerm_cdn_frontdoor_endpoint" "content" {
  name                     = "content-${var.name}"
  cdn_frontdoor_profile_id = azurerm_cdn_frontdoor_profile.this.id
}

resource "azurerm_cdn_frontdoor_origin_group" "storage" {
  name                     = "storage"
  cdn_frontdoor_profile_id = azurerm_cdn_frontdoor_profile.this.id

  load_balancing {}

  # Origin auth requires an HTTPS probe.
  health_probe {
    protocol            = "Https"
    request_type        = "HEAD"
    path                = "/"
    interval_in_seconds = 100
  }
}

# azurerm has no setting for origin auth, so this one property goes through the Azure API directly.
# With anonymous access off, the role must exist before the identity is associated. depends_on enforces that.
resource "azapi_update_resource" "origin_group_auth" {
  type        = "Microsoft.Cdn/profiles/originGroups@2025-06-01"
  resource_id = azurerm_cdn_frontdoor_origin_group.storage.id

  body = {
    properties = {
      authentication = {
        type  = "UserAssignedIdentity"
        scope = "https://storage.azure.com/.default"
        userAssignedIdentity = {
          id = azurerm_user_assigned_identity.frontdoor.id
        }
      }
    }
  }

  depends_on = [azurerm_role_assignment.frontdoor_storage]
}

resource "azurerm_cdn_frontdoor_origin" "storage" {
  name                          = "blob"
  cdn_frontdoor_origin_group_id = azurerm_cdn_frontdoor_origin_group.storage.id

  enabled                        = true
  host_name                      = azurerm_storage_account.this.primary_blob_host
  origin_host_header             = azurerm_storage_account.this.primary_blob_host
  certificate_name_check_enabled = true
}

resource "azurerm_cdn_frontdoor_secret" "wildcard" {
  name                     = "wildcard"
  cdn_frontdoor_profile_id = azurerm_cdn_frontdoor_profile.this.id

  secret {
    customer_certificate {
      key_vault_certificate_id = var.wildcard_certificate_id
    }
  }
}

resource "azurerm_cdn_frontdoor_custom_domain" "wildcard" {
  name                     = "wildcard-content"
  cdn_frontdoor_profile_id = azurerm_cdn_frontdoor_profile.this.id
  dns_zone_id              = var.dns_zone_id
  host_name                = "*.${var.content_domain}"

  tls {
    certificate_type        = "CustomerCertificate"
    cdn_frontdoor_secret_id = azurerm_cdn_frontdoor_secret.wildcard.id
  }
}

resource "azurerm_cdn_frontdoor_rule_set" "content" {
  name                     = "content"
  cdn_frontdoor_profile_id = azurerm_cdn_frontdoor_profile.this.id
}

# Without this, a hostname that fails the regex would reach the storage account with the path unchanged,
# and /u-alice/<id>/v1/index.html would work on any subdomain.
resource "azurerm_cdn_frontdoor_rule" "reject_invalid_host" {
  name                      = "RejectInvalidHost"
  cdn_frontdoor_rule_set_id = azurerm_cdn_frontdoor_rule_set.content.id
  order                     = 1
  behavior_on_match         = "Continue"

  conditions {
    host_name_condition {
      operator         = "RegEx"
      negate_condition = true
      match_values     = [local.artifact_host_regex]
      transforms       = ["Lowercase"]
    }
  }

  actions {
    url_rewrite_action {
      source_pattern          = "/"
      destination             = "/invalid-host"
      preserve_unmatched_path = false
    }
  }
}

resource "azurerm_cdn_frontdoor_rule" "artifact_path" {
  name                      = "ArtifactPath"
  cdn_frontdoor_rule_set_id = azurerm_cdn_frontdoor_rule_set.content.id
  order                     = 2
  behavior_on_match         = "Continue"

  conditions {
    host_name_condition {
      operator     = "RegEx"
      match_values = [local.artifact_host_regex]
      transforms   = ["Lowercase"]
    }
  }

  actions {
    url_rewrite_action {
      source_pattern          = "/"
      destination             = local.rewrite_destination
      preserve_unmatched_path = true
    }

    # Blob storage rejects Entra ID requests without a service version header.
    request_header_action {
      header_action = "Overwrite"
      header_name   = "x-ms-version"
      value         = "2025-07-05"
    }
  }

  depends_on = [azurerm_cdn_frontdoor_rule.reject_invalid_host]
}

resource "azurerm_cdn_frontdoor_rule" "content_headers" {
  name                      = "ContentHeaders"
  cdn_frontdoor_rule_set_id = azurerm_cdn_frontdoor_rule_set.content.id
  order                     = 3
  behavior_on_match         = "Continue"

  actions {
    # Without an explicit lifetime, Front Door caches for a random one to three days.
    route_configuration_override_action {
      cache_behavior                = "OverrideAlways"
      cache_duration                = "00:01:00"
      compression_enabled           = true
      forwarding_protocol           = "HttpsOnly"
      query_string_caching_behavior = "IgnoreQueryString"
    }

    response_header_action {
      header_action = "Overwrite"
      header_name   = "Content-Security-Policy"
      value         = local.content_security_policy
    }

    response_header_action {
      header_action = "Overwrite"
      header_name   = "X-Content-Type-Options"
      value         = "nosniff"
    }

    # Sandboxed documents have origin "null", so module scripts and fonts need CORS.
    response_header_action {
      header_action = "Overwrite"
      header_name   = "Access-Control-Allow-Origin"
      value         = "*"
    }

    # The edge caches, browsers revalidate, so a deleted artifact cannot survive in a browser cache.
    response_header_action {
      header_action = "Overwrite"
      header_name   = "Cache-Control"
      value         = "no-cache"
    }
  }

  depends_on = [azurerm_cdn_frontdoor_rule.artifact_path]
}

resource "azurerm_cdn_frontdoor_route" "content" {
  name                          = "content"
  cdn_frontdoor_endpoint_id     = azurerm_cdn_frontdoor_endpoint.content.id
  cdn_frontdoor_origin_group_id = azurerm_cdn_frontdoor_origin_group.storage.id
  cdn_frontdoor_origin_ids      = [azurerm_cdn_frontdoor_origin.storage.id]
  cdn_frontdoor_rule_set_ids    = [azurerm_cdn_frontdoor_rule_set.content.id]

  cdn_frontdoor_custom_domain_ids = [azurerm_cdn_frontdoor_custom_domain.wildcard.id]
  link_to_default_domain          = false

  patterns_to_match      = ["/*"]
  supported_protocols    = ["Https"]
  https_redirect_enabled = false
  forwarding_protocol    = "HttpsOnly"

  cache {
    query_string_caching_behavior = "IgnoreQueryString"
    compression_enabled           = true
    content_types_to_compress     = ["text/html", "text/css", "text/javascript", "application/json", "image/svg+xml"]
  }

  depends_on = [azapi_update_resource.origin_group_auth]
}

resource "azurerm_cdn_frontdoor_custom_domain_association" "wildcard" {
  cdn_frontdoor_custom_domain_id = azurerm_cdn_frontdoor_custom_domain.wildcard.id
  cdn_frontdoor_route_ids        = [azurerm_cdn_frontdoor_route.content.id]
}
