package proxy

import (
	"context"
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// AnnotationYCServiceAccount — аннотация на k8s ServiceAccount с id YC SA, как в Managed K8s.
const AnnotationYCServiceAccount = "yandex.cloud/federated-yc-service-account-id"

const (
	podIPIndex      = "podIP"
	awaitingIPIndex = "awaitingIP"
	// podIPWait — сколько ждать IP пода в informer: kubelet пишет status.podIPs на следующем syncPod после старта
	// контейнеров (PLEG relist ~1 с), запрос успевает раньше (#20). Меньше ResponseHeaderTimeout go-sdk (5 с).
	podIPWait = 3 * time.Second
)

// Identity — k8s ServiceAccount пода и привязанный к нему YC SA.
type Identity struct {
	Namespace          string
	ServiceAccount     string
	YCServiceAccountID string
}

// Resolver находит Identity пода своей ноды по его IP.
type Resolver struct {
	pods cache.SharedIndexInformer
	sas  cache.SharedIndexInformer
}

func NewResolver(client kubernetes.Interface, nodeName string) *Resolver {
	onNode := func(o *metav1.ListOptions) {
		o.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", nodeName).String()
	}
	return &Resolver{
		pods: coreinformers.NewFilteredPodInformer(client, metav1.NamespaceAll, 0,
			cache.Indexers{podIPIndex: indexPodIPs, awaitingIPIndex: indexAwaitingIP}, onNode),
		sas: coreinformers.NewServiceAccountInformer(client, metav1.NamespaceAll, 0, cache.Indexers{}),
	}
}

// Start запускает informer-ы и ждёт синка кэша.
func (r *Resolver) Start(ctx context.Context) error {
	go r.pods.RunWithContext(ctx)
	go r.sas.RunWithContext(ctx)
	if !cache.WaitForCacheSync(ctx.Done(), r.pods.HasSynced, r.sas.HasSynced) {
		return errors.New("informer cache sync failed")
	}
	return nil
}

// Resolve возвращает Identity пода с этим IP; false — пода нет, он не один или на его SA нет аннотации.
// IP нет в индексе, а на ноде есть под без IP — ждёт до podIPWait: запрос мог прийти от этого пода.
// ponytail: под, застрявший без IP (ждёт том, ошибка CNI), на всё это время задерживает 404 hostNetwork-подам и
// процессам ноды на podIPWait; сужать по времени создания пода, если это станет заметно.
func (r *Resolver) Resolve(ctx context.Context, ip string) (Identity, bool) {
	var objs []any
	wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, podIPWait, true, func(context.Context) (bool, error) {
		// Сначала поды без IP, потом индекс по IP: если IP придёт между чтениями, второе чтение его увидит.
		awaiting, _ := r.pods.GetIndexer().ByIndex(awaitingIPIndex, "")
		objs, _ = r.pods.GetIndexer().ByIndex(podIPIndex, ip)
		return len(objs) > 0 || len(awaiting) == 0, nil
	})
	if len(objs) != 1 {
		return Identity{}, false
	}
	pod := objs[0].(*corev1.Pod)
	obj, ok, _ := r.sas.GetIndexer().GetByKey(pod.Namespace + "/" + pod.Spec.ServiceAccountName)
	if !ok {
		return Identity{}, false
	}
	id := obj.(*corev1.ServiceAccount).Annotations[AnnotationYCServiceAccount]
	if id == "" {
		return Identity{}, false
	}
	return Identity{pod.Namespace, pod.Spec.ServiceAccountName, id}, true
}

// inPodNetwork — под с IP из сети подов: не hostNetwork (его IP — IP ноды) и не завершённый (его IP может получить
// новый под).
func inPodNetwork(pod *corev1.Pod) bool {
	return !pod.Spec.HostNetwork && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}

// indexAwaitingIP — поды сети подов, у которых ещё нет IP, все под ключом "".
func indexAwaitingIP(obj any) ([]string, error) {
	if pod := obj.(*corev1.Pod); inPodNetwork(pod) && len(pod.Status.PodIPs) == 0 {
		return []string{""}, nil
	}
	return nil, nil
}

func indexPodIPs(obj any) ([]string, error) {
	pod := obj.(*corev1.Pod)
	if !inPodNetwork(pod) {
		return nil, nil
	}
	var ips []string
	for _, ip := range pod.Status.PodIPs {
		ips = append(ips, ip.IP)
	}
	return ips, nil
}
