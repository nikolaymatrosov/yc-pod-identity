terraform {
  required_version = ">= 1.7"
  required_providers {
    yandex = {
      source  = "yandex-cloud/yandex"
      version = "~> 0.250"
    }
  }
}

provider "yandex" {
  folder_id = var.folder_id
}

locals {
  workloads = {
    for k, w in var.workloads : k => merge(w, {
      namespace = split("/", k)[0]
      name      = split("/", k)[1]
    })
  }

  # ns/sa -> YC SA id: заданный в записи или созданный ниже.
  service_account_ids = merge(
    { for k, w in var.workloads : k => w.service_account_id if w.service_account_id != null },
    { for k, sa in yandex_iam_service_account.workload : k => sa.id },
  )

  role_bindings = {
    for p in flatten([
      for k, w in var.workloads : [for r in w.roles : { workload = k, role = r }]
    ]) : "${p.workload}|${p.role}" => p
  }
}

resource "yandex_storage_bucket" "jwks" {
  bucket_prefix = "yc-pod-identity-jwks-"
  folder_id     = var.folder_id

  anonymous_access_flags {
    read = true
    list = false
  }
}

resource "yandex_storage_object" "jwks" {
  bucket       = yandex_storage_bucket.jwks.bucket
  key          = "jwks.json"
  source       = var.jwks_file
  source_hash  = filemd5(var.jwks_file)
  content_type = "application/json" # с text/plain IAM отвечает "Failed to fetch JWKS"
}

resource "yandex_iam_workload_identity_oidc_federation" "this" {
  name      = "yc-pod-identity"
  folder_id = var.folder_id
  issuer    = var.issuer
  audiences = [var.audience]
  jwks_url  = "https://storage.yandexcloud.net/${yandex_storage_object.jwks.bucket}/${yandex_storage_object.jwks.key}"
}

resource "yandex_iam_service_account" "workload" {
  for_each = { for k, w in local.workloads : k => w if w.service_account_id == null }

  name        = replace("${each.value.namespace}-${each.value.name}", ".", "-")
  folder_id   = var.folder_id
  description = "k8s system:serviceaccount:${each.value.namespace}:${each.value.name}"
}

resource "yandex_resourcemanager_folder_iam_member" "workload" {
  for_each = local.role_bindings

  folder_id = var.folder_id
  role      = each.value.role
  member    = "serviceAccount:${local.service_account_ids[each.value.workload]}"
}

resource "yandex_iam_workload_identity_federated_credential" "workload" {
  for_each = local.workloads

  federation_id       = yandex_iam_workload_identity_oidc_federation.this.id
  service_account_id  = local.service_account_ids[each.key]
  external_subject_id = "system:serviceaccount:${each.value.namespace}:${each.value.name}"
}
