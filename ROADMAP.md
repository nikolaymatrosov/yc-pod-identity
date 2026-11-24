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
- Кэш IAM-токенов в памяти по `(namespace, serviceaccount, id YC SA)`; `expires_in` — реальный остаток TTL
  (IAM-токен живёт 12 ч). `yandex-cloud/go-sdk` опрашивает метаданные каждые ~50 с, поэтому и кэш, и чтение
  SA через informer (без запроса к API на каждый вызов) обязательны.
- Deny-политика на `169.254.169.254` обязательна: LRP fail-open, пока на ноде нет готового пода прокси (фаза 0).
- hostNetwork-поды и процессы ноды перехват тоже затрагивает (socket LB действует и в host namespace, фаза 4).
  Прокси узнаёт их по IP `cilium_host` (шлюз по умолчанию своего пода) и пересылает `GET` на пути allowlist в
  настоящие метаданные (#14); `skipRedirectFromBackend` в LRP, L7-политика Envoy на egress прокси, deny исключает
  SA прокси. Без прокси на ноде (deny host namespace не выбирает) — настоящие метаданные.

## Фазы

### Фаза 0 — спайк Cilium LRP (без YC, без Go)

Снять главные неизвестные на тестовом nginx до написания кода. Выход — таблица результатов в
`docs/lrp-spike.md`.

- [x] Включить `localRedirectPolicies.enabled` + `addressMatcherCIDRs: [169.254.169.254/32]` в values Cilium.
- [x] Перехват работает: под ходит на `169.254.169.254:80`, попадает в nginx на своей ноде; source IP пода сохраняется.
- [x] Бэкенд обычный CNI-под (не hostNetwork): прокси не ходит в реальные метаданные, петли нет — проверить, что `skipRedirectFromBackend` не нужен.
- [x] Поведение без готового бэкенда на ноде: fail-open или fail-closed. Если fail-open — deny-политика обязана закрывать дыру; проверить совместно.
- [x] Порядок LRP и CiliumNetworkPolicy: перенаправленный трафик проходит к прокси при включённой deny-политике.

### Фаза 1 — федерация в Terraform

WIF end-to-end без Go уже проверен вручную (`yc` + curl) в yc-talos: `secrets.md`, вариант 3, и `ops.md` §1.3.
Осталось перенести эту настройку в Terraform.

Что там выяснено:

- **Публиковать issuer не нужно.** Федерации передаётся `jwks_url` — JWKS лежит в публичном бакете Object Storage,
  к API-серверу IAM не ходит (6443 был открыт только для одного IP). Issuer остаётся `https://<IP>:6443` из
  `KubeServiceAccountConfig.issuerURL`.
- JWKS отдаётся с `Content-Type: application/json`, иначе IAM отвечает `Failed to fetch JWKS`.
- Ключ подписи в Talos статический (`issuer.privateKey`): JWKS перезаливается только при его ротации.
- Токен должен быть с audience федерации; с audience по умолчанию — `Jwt has no matching audience`.
- Обмен RFC 8693 на `auth.yandex.cloud/oauth/token` отдаёт `{"access_token", "expires_in": 43200, "token_type"}` —
  уже формат GCE metadata.
- Пара «SA + subject» связывается только с одним issuer: вторая федерация на тот же subject — `AlreadyExists`.
  Федерация `talos-ops-k8s` (audience `ycaud`), созданная на стенде через `yc`, живёт рядом с федерацией из
  Terraform с тем же issuer: конфликт только при той же паре SA + subject.

- [x] `terraform/`: бакет + `yandex_storage_object` с JWKS (`content_type = "application/json"`),
  `yandex_iam_workload_identity_oidc_federation` (`issuer`, `audiences`, `jwks_url`),
  `yandex_iam_workload_identity_federated_credential` (`external_subject_id` = `system:serviceaccount:<ns>:<sa>`),
  тестовый YC SA с ролью. Привязки — map `workloads` (существующий или создаваемый YC SA, роли на каталог).
- [x] Проверить федерацию из Terraform тем же обменом: `kubectl create token --audience <aud>` → curl на
  `auth.yandex.cloud/oauth/token` → IAM-токен → вызов YC API.

### Фаза 2 — прокси на Go

- [x] Скелет: `cmd/metadata-proxy`, `internal/proxy` (resolver, exchange, server). Зависимости: client-go + stdlib, без фреймворков.
- [x] Резолвер: informer по подам ноды, индекс по podIP, informer по SA кластера для чтения аннотации.
- [x] TokenRequest + обмен RFC 8693 + кэш токенов.
- [x] HTTP: token endpoint в формате GCE, проверка `Metadata-Flavor`, 404 на остальное, `/healthz`.
- [x] Unit-тесты: fake clientset + httptest (резолв по IP, под без аннотации, кэш-хит, протухший токен).

### Фаза 3 — поставка: образ + Helm chart

- [x] Образ: multi-stage Dockerfile, distroless/static, nonroot.
- [x] `charts/metadata-proxy`: DaemonSet, ServiceAccount + RBAC (list/watch pods и serviceaccounts; create serviceaccounts/token), CiliumLocalRedirectPolicy, values (audience), ValidatingAdmissionPolicy (TokenRequest только с audience федерации). `federation_id` прокси не нужен. Аргументы: `-node-name=$(NODE_NAME)` из downward API, `-audience`, `-listen` (`:8080`, бэкенд-порт LRP фиксирован).
- [x] Документация установки: values Cilium (фаза 0) + chart + terraform outputs → values чарта.

### Фаза 4 — e2e на Talos-кластере

Таблица результатов в `docs/e2e.md`, манифесты и клиент на go-sdk — `e2e/`:

- [x] Под с аннотированным SA получает IAM-токен штатным `yandex-cloud/go-sdk` без переменных окружения и сайдкаров.
- [x] Под без аннотации — отказ, токен VM не утекает.
- [x] hostNetwork-под: пока прокси на ноде готов, **не** видит настоящие метаданные — LRP перехватывает и host
  namespace, прокси отвечает 404; без прокси видит (deny host namespace не выбирает). hostNetwork-софт с метаданными
  (CSI YC) работает с перебоями — #14.
- [x] Убить прокси на ноде: трафик упирается в deny-политику, после рестарта всё восстанавливается.
- [x] Ротация: истечение токена — unit-тест; на стенде — потеря кэша при рестарте прокси и рестарт ноды.
- [x] Раскатка и рестарт ноды. `maxSurge: 1` давал раскатку без окна, но при выключении ноды — шквал отклонённых
  kubelet подов (110 за 5 с); чарт переведён на `maxUnavailable: 1` — окно ~10 с на ноду закрывает deny.
- [x] VAP режет TokenRequest от SA прокси с чужой audience.

### Passthrough для host namespace (#14)

Результаты — `docs/e2e-host-passthrough.md`.

- [x] Прокси пересылает запросы с IP `cilium_host` на пути allowlist в настоящие метаданные; LRP
  `skipRedirectFromBackend`, deny по SA, L7-allowlist (чарт 0.2.0).
- [ ] e2e: hostNetwork-под видит пути allowlist, остальное — 404; прокси из своего netns получает только allowlist
  (Envoy); раскатки и рестарт агента Cilium не ломают skip (cilium/cilium#40450).

### Фаза 5 — эксплуатация

- [ ] Prometheus-метрики (запросы, ошибки обмена, размер кэша). Логи slog уже есть (фаза 2).
- [ ] Graceful shutdown (сейчас на SIGTERM — exit 2, фаза 4). Readiness частично закрыт в фазе 2: порт открывается
  только после синка informer-ов.
- [ ] Окно раскатки (~10 с на ноду, фаза 4) в основном задаёт `periodSeconds: 10` readinessProbe по умолчанию —
  уменьшить и перемерить.

## Вне скоупа MVP — и зачем может пригодиться

| Что | Зачем вернуться |
| --- | --- |
| `/service-accounts/default/email`, `/identity` | GCP-клиентские библиотеки читают email/identity до запроса токена; понадобится, если в подах появится софт на google-auth. |
| Остальные пути метаданных (instance id, attributes, user-data) | Софт, который детектит «я в облаке» или читает конфиг из attributes. Добавлять по конкретным падениям, не впрок. |
| Passthrough произвольных путей к реальному `169.254.169.254` | Максимальная совместимость, но это дыра в изоляции (под увидит метаданные VM); только как осознанная опция. Для host namespace сделан allowlist (#14). |
| Аннотация на Pod вместо SA / несколько YC SA | Когда одному k8s SA понадобятся разные YC SA. |
| Префетч/фоновое обновление токенов | Когда первый запрос после протухания станет заметен по латентности. |

## Риски

- **Fail-open LRP** — подтверждён (пода прокси на ноде нет или он NotReady): запрос уходит в настоящие метаданные (`docs/lrp-spike.md`). Deny-политика — обязательная часть установки, не опция: без неё упавший прокси открывает user-data с CA-ключами.
- **Публичность issuer** — снят: IAM берёт ключи по `jwks_url` из Object Storage. Остаётся операционное: при
  ротации ключа подписи Talos перезалить JWKS, иначе обмен токенов перестанет работать.
- **Прокси — доверенная точка выдачи токенов**: RBAC `create serviceaccounts/token` на кластер не ограничен по audience — VAP чарта пропускает от SA прокси только TokenRequest с audience федерации; взлом прокси = токены любой SA с audience федерации (все федеративные SA в YC) и чтение всех спеков подов (list/watch pods), но не API кластера. `addressMatcherCIDRs` ограничивает LRP; токены только подам своей ноды (это обеспечивает код прокси, не RBAC).
- Перехват работает только при socket LB (`kubeProxyReplacement: true`) — в кластере уже так. Обратная сторона:
  socket LB перехватывает и host namespace; запросы хоста прокси пересылает по allowlist (#14).
- **Прокси с доступом к метаданным ВМ** (#14): ради пересылки запросов хоста под прокси ходит в
  `169.254.169.254:80`. Взлом прокси даёт пути allowlist (ID ВМ, зона, hostname); токен SA ВМ и user-data закрыты
  Envoy. `skipRedirectFromBackend` имеет известные баги (cilium/cilium#36740, #40450) — проверяется в e2e.
