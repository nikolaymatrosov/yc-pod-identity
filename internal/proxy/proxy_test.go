package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const testNode = "node-a"

// testHostIP — cilium_host ноды в тестах: с него приходят запросы host namespace. Подов с этим IP в fixture нет.
const testHostIP = "10.0.0.254"

var testHostPaths = []string{"/latest/dynamic/instance-identity/document", "/computeMetadata/v1/instance/id"}

func pod(name, ip, sa string, mutate ...func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
		Spec:       corev1.PodSpec{NodeName: testNode, ServiceAccountName: sa},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIPs: []corev1.PodIP{{IP: ip}}},
	}
	for _, m := range mutate {
		m(p)
	}
	return p
}

func serviceAccount(name, ycSA string) *corev1.ServiceAccount {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name}}
	if ycSA != "" {
		sa.Annotations = map[string]string{AnnotationYCServiceAccount: ycSA}
	}
	return sa
}

type fixture struct {
	client   *fake.Clientset
	resolver *Resolver
	server   *Server
	iamCalls atomic.Int32

	upstream       *url.URL
	upstreamCalls  atomic.Int32
	upstreamHeader atomic.Pointer[http.Header]
}

// newFixture: поды и SA в namespace ns; IAM отвечает по audience (id YC SA):
// sa-broken — 400 invalid_client, sa-short — expires_in 60, остальные — токен iam-<id> на 12 ч.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{client: fake.NewClientset(
		pod("app", "10.0.0.1", "app"),
		pod("plain", "10.0.0.2", "plain"),
		pod("host", "10.0.0.3", "app", func(p *corev1.Pod) { p.Spec.HostNetwork = true }),
		pod("done", "10.0.0.4", "app", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }),
		pod("short", "10.0.0.5", "short"),
		pod("broken", "10.0.0.6", "broken"),
		serviceAccount("app", "sa-app"),
		serviceAccount("plain", ""),
		serviceAccount("short", "sa-short"),
		serviceAccount("broken", "sa-broken"),
	)}

	f.client.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "token" {
			return false, nil, nil
		}
		tr := a.(k8stesting.CreateAction).GetObject().(*authv1.TokenRequest)
		if !slices.Equal(tr.Spec.Audiences, []string{"yc-pod-identity"}) {
			t.Errorf("TokenRequest audiences = %v", tr.Spec.Audiences)
		}
		return true, &authv1.TokenRequest{Status: authv1.TokenRequestStatus{Token: "k8s-jwt"}}, nil
	})

	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.iamCalls.Add(1)
		r.ParseForm()
		if got := r.PostForm.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:token-exchange" {
			t.Errorf("grant_type = %q", got)
		}
		if got := r.PostForm.Get("subject_token"); got != "k8s-jwt" {
			t.Errorf("subject_token = %q", got)
		}
		switch aud := r.PostForm.Get("audience"); aud {
		case "sa-broken":
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"invalid_client","error_description":"No matching federated credentials"}`)
		case "sa-short":
			io.WriteString(w, `{"access_token":"iam-sa-short","expires_in":60,"token_type":"Bearer"}`)
		default:
			fmt.Fprintf(w, `{"access_token":"iam-%s","expires_in":43200,"token_type":"Bearer"}`, aud)
		}
	}))
	t.Cleanup(iam.Close)

	// Настоящие метаданные: отвечают "real <path>" и ставят свой Metadata-Flavor.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.upstreamCalls.Add(1)
		h := r.Header.Clone()
		f.upstreamHeader.Store(&h)
		w.Header().Set("Metadata-Flavor", "Google")
		fmt.Fprintf(w, "real %s", r.URL.Path)
	}))
	t.Cleanup(up.Close)
	f.upstream, _ = url.Parse(up.URL)

	f.resolver = NewResolver(f.client, testNode)
	if err := f.resolver.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	ex := NewExchanger(f.client, "yc-pod-identity")
	ex.tokenURL = iam.URL
	f.server = NewServer(f.resolver, ex, HostPassthrough{IP: testHostIP, Paths: testHostPaths, Upstream: f.upstream})
	return f
}

func tokenRequest(ip string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, tokenPath, nil)
	req.RemoteAddr = ip + ":40000"
	req.Header.Set("Metadata-Flavor", "Google")
	return req
}

func (f *fixture) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.server.ServeHTTP(rec, req)
	return rec
}

func TestToken(t *testing.T) {
	f := newFixture(t)
	rec := f.do(tokenRequest("10.0.0.1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body %q", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Metadata-Flavor"); got != "Google" {
		t.Errorf("Metadata-Flavor = %q", got)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.AccessToken != "iam-sa-app" || body.TokenType != "Bearer" || body.ExpiresIn < 43000 || body.ExpiresIn > 43200 {
		t.Errorf("body = %+v", body)
	}
}

func TestNoIdentity(t *testing.T) {
	f := newFixture(t)
	for name, ip := range map[string]string{
		"unknown IP":    "10.0.0.99",
		"no annotation": "10.0.0.2",
		"hostNetwork":   "10.0.0.3",
		"succeeded":     "10.0.0.4",
	} {
		if rec := f.do(tokenRequest(ip)); rec.Code != http.StatusNotFound {
			t.Errorf("%s: code = %d", name, rec.Code)
		}
	}
	if n := f.iamCalls.Load(); n != 0 {
		t.Errorf("IAM calls = %d", n)
	}
}

func TestForbidden(t *testing.T) {
	f := newFixture(t)
	noFlavor := tokenRequest("10.0.0.1")
	noFlavor.Header.Del("Metadata-Flavor")
	forwarded := tokenRequest("10.0.0.1")
	forwarded.Header.Set("X-Forwarded-For", "1.2.3.4")
	for name, req := range map[string]*http.Request{"no Metadata-Flavor": noFlavor, "X-Forwarded-For": forwarded} {
		if rec := f.do(req); rec.Code != http.StatusForbidden {
			t.Errorf("%s: code = %d", name, rec.Code)
		}
	}
}

func TestCache(t *testing.T) {
	f := newFixture(t)
	f.do(tokenRequest("10.0.0.1"))
	f.do(tokenRequest("10.0.0.1"))
	if n := f.iamCalls.Load(); n != 1 {
		t.Errorf("IAM calls = %d, want 1", n)
	}
}

func TestShortLivedTokenNotCached(t *testing.T) {
	f := newFixture(t)
	f.do(tokenRequest("10.0.0.5"))
	f.do(tokenRequest("10.0.0.5"))
	if n := f.iamCalls.Load(); n != 2 {
		t.Errorf("IAM calls = %d, want 2", n)
	}
}

func TestExchangeError(t *testing.T) {
	f := newFixture(t)
	rec := f.do(tokenRequest("10.0.0.6"))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "invalid_client") {
		t.Errorf("code = %d, body %q", rec.Code, rec.Body)
	}
}

func TestAnnotationRemoved(t *testing.T) {
	f := newFixture(t)
	if rec := f.do(tokenRequest("10.0.0.1")); rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if _, err := f.client.CoreV1().ServiceAccounts("ns").Update(t.Context(), serviceAccount("app", ""), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, ok := f.resolver.Resolve("10.0.0.1"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("informer did not see the annotation removal")
		}
	}
	if rec := f.do(tokenRequest("10.0.0.1")); rec.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404 after annotation removal", rec.Code)
	}
}

func TestPodsListedForOwnNodeOnly(t *testing.T) {
	f := newFixture(t)
	for _, a := range f.client.Actions() {
		if l, ok := a.(k8stesting.ListAction); ok && a.GetResource().Resource == "pods" {
			if got := l.GetListRestrictions().Fields.String(); got != "spec.nodeName="+testNode {
				t.Errorf("pods list field selector = %q", got)
			}
			return
		}
	}
	t.Error("no pods list action")
}

func TestHealthz(t *testing.T) {
	f := newFixture(t)
	rec := f.do(httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d", rec.Code)
	}
	if rec := f.do(httptest.NewRequest(http.MethodGet, "/computeMetadata/v1/instance/id", nil)); rec.Code != http.StatusNotFound {
		t.Errorf("other path code = %d", rec.Code)
	}
}

func hostRequest(method, target string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = testHostIP + ":40000"
	return req
}

func TestHostPassthrough(t *testing.T) {
	f := newFixture(t)
	req := hostRequest(http.MethodGet, "/computeMetadata/v1/instance/id")
	req.Header.Set("Metadata-Flavor", "Google")
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	rec := f.do(req)
	if rec.Code != http.StatusOK || rec.Body.String() != "real /computeMetadata/v1/instance/id" {
		t.Fatalf("code = %d, body %q", rec.Code, rec.Body)
	}
	if got := rec.Header().Values("Metadata-Flavor"); len(got) != 1 {
		t.Errorf("response Metadata-Flavor = %q, want exactly one", got)
	}
	h := *f.upstreamHeader.Load()
	if got := h.Get("Metadata-Flavor"); got != "Google" {
		t.Errorf("upstream Metadata-Flavor = %q", got)
	}
	if got := h.Get("X-Forwarded-For"); got != "" {
		t.Errorf("upstream X-Forwarded-For = %q", got)
	}
	if rec := f.do(hostRequest(http.MethodGet, "/healthz")); rec.Code != http.StatusOK {
		t.Errorf("healthz from host: code = %d", rec.Code)
	}
}

func TestHostPassthroughRejected(t *testing.T) {
	f := newFixture(t)
	hostToken := hostRequest(http.MethodGet, tokenPath)
	hostToken.Header.Set("Metadata-Flavor", "Google")
	fromPod := httptest.NewRequest(http.MethodGet, "/computeMetadata/v1/instance/id", nil)
	fromPod.RemoteAddr = "10.0.0.1:40000"
	for name, req := range map[string]*http.Request{
		"user-data":    hostRequest(http.MethodGet, "/latest/user-data"),
		"query":        hostRequest(http.MethodGet, "/computeMetadata/v1/instance/id?alt=json"),
		"escaped path": hostRequest(http.MethodGet, "/computeMetadata/v1/instance/i%64"),
		"POST":         hostRequest(http.MethodPost, "/computeMetadata/v1/instance/id"),
		"token":        hostToken,
		"pod IP":       fromPod,
	} {
		if rec := f.do(req); rec.Code != http.StatusNotFound {
			t.Errorf("%s: code = %d", name, rec.Code)
		}
	}
	if n := f.upstreamCalls.Load(); n != 0 {
		t.Errorf("upstream calls = %d", n)
	}
}

func TestHostPassthroughDisabled(t *testing.T) {
	f := newFixture(t)
	for name, host := range map[string]HostPassthrough{
		"no paths": {IP: testHostIP, Upstream: f.upstream},
		"no IP":    {Paths: testHostPaths, Upstream: f.upstream},
	} {
		rec := httptest.NewRecorder()
		NewServer(f.resolver, nil, host).ServeHTTP(rec, hostRequest(http.MethodGet, "/computeMetadata/v1/instance/id"))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: code = %d", name, rec.Code)
		}
	}
	if n := f.upstreamCalls.Load(); n != 0 {
		t.Errorf("upstream calls = %d", n)
	}
}
