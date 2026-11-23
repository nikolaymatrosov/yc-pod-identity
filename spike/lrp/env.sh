# Окружение спайка LRP. Подключать в каждом блоке команд из корня репо: . spike/lrp/env.sh
export KUBECONFIG=$HOME/experiments/yc-talos/_work/ops/kubeconfig

A=$(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{.items[0].metadata.name}')
B=$(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{.items[1].metadata.name}')

pod()   { kubectl -n "$1" get pod -l "app=$2" --field-selector "spec.nodeName=$3" -o jsonpath='{.items[0].metadata.name}'; }
agent() { kubectl -n kube-system get pod -l k8s-app=cilium --field-selector "spec.nodeName=$1" -o jsonpath='{.items[0].metadata.name}'; }

# Только instance/id: user-data и token в спайке не запрашиваем.
probe() {
  kubectl -n lrp-client exec "$(pod lrp-client lrp-client "$1")" -- \
    curl -s --max-time 5 -w ' http=%{http_code} t=%{time_total}\n' -H 'Metadata-Flavor: Google' \
    http://169.254.169.254/computeMetadata/v1/instance/id
  echo "exit=$?"
}
flows()    { kubectl -n kube-system exec "$(agent "$1")" -c cilium-agent -- hubble observe --from-pod "$2" --last 30; }
lrpstate() { kubectl -n kube-system exec "$(agent "$1")" -c cilium-agent -- sh -c 'cilium-dbg lrp list; cilium-dbg service list | grep -E "^ID|169.254.169.254"'; }
