# Окружение e2e (фаза 4). Подключать в каждом блоке команд из корня репо: . e2e/env.sh
export KUBECONFIG=$HOME/experiments/yc-talos/_work/ops/kubeconfig
export TALOSCONFIG=$HOME/experiments/yc-talos/_work/ops/talosconfig

NS=yc-pod-identity
# Нода A — воркер с меткой yc-pod-identity-e2e=target (ставится один раз при подготовке).
A=$(kubectl get nodes -l yc-pod-identity-e2e=target -o jsonpath='{.items[0].metadata.name}')
A_IP=$(kubectl get node "$A" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
FOLDER=$(awk -F'"' '/^folder_id/{print $2}' terraform/terraform.tfvars)

proxy() { kubectl -n "$NS" get pod -l app.kubernetes.io/name=metadata-proxy --field-selector "spec.nodeName=$A" -o jsonpath='{.items[0].metadata.name}'; }
agent() { kubectl -n kube-system get pod -l k8s-app=cilium --field-selector "spec.nodeName=$A" -o jsonpath='{.items[0].metadata.name}'; }
flows() { kubectl -n kube-system exec "$(agent)" -c cilium-agent -- hubble observe --from-pod "$1" --last "${2:-30}"; }
