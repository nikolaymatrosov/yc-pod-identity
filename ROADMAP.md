# Roadmap: metadata-прокси для WIF на self-hosted Kubernetes (Talos + Cilium)

## Цель

DaemonSet-прокси на Go, который через CiliumLocalRedirectPolicy перехватывает запросы подов к
`169.254.169.254` и выдаёт каждому поду IAM-токен Yandex Cloud его собственного сервисного
аккаунта — через Workload Identity Federation, в формате GCE metadata. `yandex-cloud/go-sdk`
с жёстко прошитым адресом метаданных работает без изменений.

## Prior art (проверено 2026-11-23)

- **Официального продукта нет.** Аналогичный механизм существует только внутри
  [Managed Service for Kubernetes](https://yandex.cloud/ru/docs/managed-kubernetes/tutorials/wlif-managed-k8s-integration):
  там кластер сам даёт metadata-endpoint и аннотацию `yandex.cloud/federated-yc-service-account-id`.
  На self-hosted это недоступно — официальный путь только ручной обмен JWT через федерацию.
  Мы повторяем UX managed-решения, включая ту же аннотацию.
- [itlabsio/yc-wlif](https://github.com/itlabsio/yc-wlif) — Go-библиотека обмена k8s-токена на
  IAM-токен (RFC 8693, `auth.yandex.cloud/oauth/token`). Не прокси: нет сервера, нет TokenRequest
  под чужие SA, лицензия не указана, 0 звёзд. Используем как референс формата запроса обмена,
  сам обмен — ~50 строк на stdlib.

## Архитектура (решения зафиксированы)

Путь запроса: под → `connect(169.254.169.254:80)` → Cilium socket LB (LRP) → прокси-под на той же
ноде → по source IP находим под → на его ServiceAccount читаем аннотацию
`yandex.cloud/federated-yc-service-account-id` → TokenRequest (audience федерации) → обмен
RFC 8693 на `https://auth.yandex.cloud/oauth/token` → ответ в формате GCE
`{"access_token": ..., "expires_in": ..., "token_type": "Bearer"}`.

- MVP отвечает **только** на `/computeMetadata/v1/instance/service-accounts/default/token`
  (плюс health). Остальные пути — 404. Требуем заголовок `Metadata-Flavor: Google`.
- Резолв пода: informer на поды своей ноды (fieldSelector `spec.nodeName`), индекс по
  `status.podIP`. Под не найден или без аннотации — 404, токен не выдаём.
- Кэш IAM-токенов в памяти по `(namespace, serviceaccount)`; `expires_in` — реальный остаток TTL
  (IAM-токен живёт 12 ч).
- Deny-политика на `169.254.169.254` остаётся как страховка на случай падения прокси.
- hostNetwork-поды (CSI, Talos) перехват не затрагивает — они видят настоящие метаданные.

## Фазы

### Фаза 0 — спайк Cilium LRP (без YC, без Go)

Снять главные неизвестные на тестовом nginx до написания кода. Выход — таблица результатов в
`docs/lrp-spike.md`.

- [ ] Включить `localRedirectPolicies.enabled` + `addressMatcherCIDRs: [169.254.169.254/32]` в values Cilium.
- [ ] Перехват работает: под ходит на `169.254.169.254:80`, попадает в nginx на своей ноде; source IP пода сохраняется.
- [ ] Бэкенд обычный CNI-под (не hostNetwork): прокси не ходит в реальные метаданные, петли нет — проверить, что `skipRedirectFromBackend` не нужен.
- [ ] Поведение без готового бэкенда на ноде: fail-open или fail-closed. Если fail-open — deny-политика обязана закрывать дыру; проверить совместно.
- [ ] Порядок LRP и CiliumNetworkPolicy: перенаправленный трафик проходит к прокси при включённой deny-политике.

### Фаза 1 — федерация: Terraform + ручной обмен

Доказать WIF end-to-end без единой строчки Go. Главный риск фазы: **issuer Talos-кластера должен
быть доступен Yandex Cloud снаружи** (OIDC discovery + JWKS). Варианты: anonymous-auth RBAC на
`/.well-known/openid-configuration` и `/openid/v1/jwks` за публичным LB, либо статический issuer в
Object Storage + `service-account-issuer`/`jwks-uri` в конфиге Talos.

- [ ] `terraform/`: `yandex_iam_workload_identity_oidc_federation` (issuer, audience) + `yandex_iam_workload_identity_federated_credential` (external_subject_id = `system:serviceaccount:<ns>:<sa>`) + тестовый YC SA с ролью.
- [ ] Опубликовать issuer кластера, зафиксировать выбранный вариант в `docs/issuer.md`.
- [ ] Руками: `kubectl create token` с audience федерации → curl на `auth.yandex.cloud/oauth/token` → IAM-токен → вызов YC API. Записать рабочий запрос в доку.

### Фаза 2 — прокси на Go

- [ ] Скелет: `cmd/metadata-proxy`, `internal/` (server, resolver, exchange). Зависимости: client-go + stdlib, без фреймворков.
- [ ] Резолвер: informer по подам ноды, индекс по podIP, чтение аннотации SA.
- [ ] TokenRequest + обмен RFC 8693 + кэш токенов.
- [ ] HTTP: token endpoint в формате GCE, проверка `Metadata-Flavor`, 404 на остальное, `/healthz`.
- [ ] Unit-тесты: fake clientset + httptest (резолв по IP, под без аннотации, кэш-хит, протухший токен).

### Фаза 3 — поставка: образ + Helm chart

- [ ] Образ: multi-stage Dockerfile, distroless/static, nonroot.
- [ ] `charts/metadata-proxy`: DaemonSet, ServiceAccount + RBAC (list/watch pods; get serviceaccounts; create serviceaccounts/token), CiliumLocalRedirectPolicy, values (federation_id, audience, порт).
- [ ] Документация установки: values Cilium (фаза 0) + chart + terraform outputs → values чарта.

### Фаза 4 — e2e на Talos-кластере

Таблица результатов в `docs/e2e.md`:

- [ ] Под с аннотированным SA получает IAM-токен штатным `yandex-cloud/go-sdk` без переменных окружения и сайдкаров.
- [ ] Под без аннотации — отказ, токен VM не утекает.
- [ ] hostNetwork-под видит настоящие метаданные.
- [ ] Убить прокси на ноде: трафик упирается в deny-политику, после рестарта всё восстанавливается.
- [ ] Ротация: токен в кэше протухает и обновляется; рестарт ноды.

### Фаза 5 — эксплуатация

- [ ] Prometheus-метрики (запросы, ошибки обмена, размер кэша) + structured logs (slog).
- [ ] Graceful shutdown, readiness с учётом синка informer-а.

## Вне скоупа MVP — и зачем может пригодиться

| Что | Зачем вернуться |
| --- | --- |
| `/service-accounts/default/email`, `/identity` | GCP-клиентские библиотеки читают email/identity до запроса токена; понадобится, если в подах появится софт на google-auth. |
| Остальные пути метаданных (instance id, attributes, user-data) | Софт, который детектит «я в облаке» или читает конфиг из attributes. Добавлять по конкретным падениям, не впрок. |
| Passthrough неизвестных путей к реальному `169.254.169.254` | Максимальная совместимость, но это дыра в изоляции (под увидит метаданные VM); только как осознанная опция. |
| Аннотация на Pod вместо SA / несколько YC SA | Когда одному k8s SA понадобятся разные YC SA. |
| Префетч/фоновое обновление токенов | Когда первый запрос после протухания станет заметен по латентности. |

## Риски

- **Fail-open LRP** — неизвестно, проверяется в фазе 0; страховка — deny-политика.
- **Публичность issuer** — без неё федерация не работает вовсе; решается в фазе 1 до любого кода.
- **Прокси — доверенная точка выдачи токенов**: RBAC минимальный, `addressMatcherCIDRs` ограничивает LRP, токены только подам своей ноды.
- Перехват работает только при socket LB (`kubeProxyReplacement: true`) — в кластере уже так.
