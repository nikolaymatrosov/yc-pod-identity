variable "folder_id" {
  description = "Каталог Yandex Cloud: федерация, бакет JWKS, создаваемые SA и их роли."
  type        = string
}

variable "issuer" {
  description = "Issuer SA-токенов кластера: kubectl get --raw /.well-known/openid-configuration | jq -r .issuer"
  type        = string
}

variable "jwks_file" {
  description = "JWKS кластера: kubectl get --raw /openid/v1/jwks > jwks.json. Перезаливать при ротации ключа подписи SA-токенов."
  type        = string
  default     = "jwks.json"
}

variable "audience" {
  description = "Audience федерации; с ней прокси запрашивает TokenRequest."
  type        = string
  default     = "yc-pod-identity"
}

variable "workloads" {
  description = <<-EOT
    k8s ServiceAccount "<namespace>/<name>" -> YC SA. service_account_id задан — берётся существующий SA,
    не задан — SA создаётся. roles выдаются итоговому SA на каталог folder_id.
  EOT
  type = map(object({
    service_account_id = optional(string)
    roles              = optional(list(string), [])
  }))
  default = {}

  validation {
    condition     = alltrue([for k in keys(var.workloads) : can(regex("^[a-z0-9]([-a-z0-9]*[a-z0-9])?/[a-z0-9]([-.a-z0-9]*[a-z0-9])?$", k))])
    error_message = "Ключи workloads — \"<namespace>/<serviceaccount>\"."
  }
}
