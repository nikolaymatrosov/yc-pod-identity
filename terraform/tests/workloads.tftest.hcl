mock_provider "yandex" {
  mock_resource "yandex_iam_service_account" {
    defaults = { id = "sa-created" }
  }
}

variables {
  folder_id = "folder"
  issuer    = "https://10.0.0.1:6443"
  jwks_file = "tests/jwks.json"
}

run "existing_and_created_sa" {
  variables {
    workloads = {
      "ns1/existing" = { service_account_id = "sa-existing", roles = ["viewer"] }
      "ns2/new.app"  = { roles = ["resource-manager.viewer", "viewer"] }
    }
  }

  assert {
    condition     = output.service_account_ids == { "ns1/existing" = "sa-existing", "ns2/new.app" = "sa-created" }
    error_message = "service_account_ids: ${jsonencode(output.service_account_ids)}"
  }

  assert {
    condition     = keys(yandex_iam_service_account.workload) == ["ns2/new.app"] && yandex_iam_service_account.workload["ns2/new.app"].name == "ns2-new-app"
    error_message = "SA создаётся только для записи без service_account_id, имя ns2-new-app"
  }

  assert {
    condition     = yandex_iam_workload_identity_federated_credential.workload["ns2/new.app"].external_subject_id == "system:serviceaccount:ns2:new.app"
    error_message = "subject credential"
  }

  assert {
    condition     = yandex_resourcemanager_folder_iam_member.workload["ns1/existing|viewer"].member == "serviceAccount:sa-existing" && length(yandex_resourcemanager_folder_iam_member.workload) == 3
    error_message = "роли: одна запись на пару (workload, role)"
  }

  assert {
    condition     = yandex_storage_object.jwks.content_type == "application/json"
    error_message = "JWKS обязан отдаваться с Content-Type application/json"
  }
}

run "bad_key" {
  command = plan

  variables {
    workloads = { "no-slash" = {} }
  }

  expect_failures = [var.workloads]
}
