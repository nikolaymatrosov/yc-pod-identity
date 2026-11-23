output "federation_id" {
  value = yandex_iam_workload_identity_oidc_federation.this.id
}

output "audience" {
  value = var.audience
}

output "jwks_url" {
  value = yandex_iam_workload_identity_oidc_federation.this.jwks_url
}

output "service_account_ids" {
  description = "\"ns/sa\" -> YC SA id, значение аннотации yandex.cloud/federated-yc-service-account-id на k8s SA."
  value       = local.service_account_ids
}
