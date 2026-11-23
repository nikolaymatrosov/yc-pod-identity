package proxy

import (
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// AnnotationYCServiceAccount — аннотация на k8s ServiceAccount с id YC SA, как в Managed K8s.
const AnnotationYCServiceAccount = "yandex.cloud/federated-yc-service-account-id"

const podIPIndex = "podIP"

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
			cache.Indexers{podIPIndex: indexPodIPs}, onNode),
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
func (r *Resolver) Resolve(ip string) (Identity, bool) {
	objs, _ := r.pods.GetIndexer().ByIndex(podIPIndex, ip)
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

// indexPodIPs не индексирует hostNetwork-поды (их IP — IP ноды) и завершённые (их IP может получить новый под).
func indexPodIPs(obj any) ([]string, error) {
	pod := obj.(*corev1.Pod)
	if pod.Spec.HostNetwork || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return nil, nil
	}
	var ips []string
	for _, ip := range pod.Status.PodIPs {
		ips = append(ips, ip.IP)
	}
	return ips, nil
}
