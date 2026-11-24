package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"sync"
	"time"
)

const (
	tokenPath = "/computeMetadata/v1/instance/service-accounts/default/token"
	// minValidity — токен из кэша отдаётся, только если до истечения больше этого.
	minValidity = 5 * time.Minute
)

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// HostPassthrough — пересылка запросов host namespace (hostNetwork-поды, процессы ноды) в настоящие метаданные ВМ.
// Socket LB отдаёт прокси и их connect() к 169.254.169.254, с IP источника cilium_host своей ноды. Пустые IP или
// Paths выключают пересылку.
type HostPassthrough struct {
	IP       string   // IP cilium_host своей ноды
	Paths    []string // точные request-target: путь без query и без нестандартного экранирования
	Upstream *url.URL // настоящие метаданные; connect() самого прокси LRP не перехватывает (skipRedirectFromBackend)
}

// Server отвечает на token endpoint GCE metadata IAM-токеном SA пода-клиента.
type Server struct {
	resolver  *Resolver
	exchanger *Exchanger
	host      HostPassthrough
	upstream  http.Handler

	mu    sync.Mutex
	cache map[Identity]cachedToken
}

func NewServer(r *Resolver, e *Exchanger, host HostPassthrough) *Server {
	return &Server{
		resolver:  r,
		exchanger: e,
		host:      host,
		// Rewrite, в отличие от Director, не добавляет X-Forwarded-For и вычищает присланный клиентом.
		upstream: &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) { pr.SetURL(host.Upstream) },
			ModifyResponse: func(resp *http.Response) error {
				slog.Info("host passthrough", "path", resp.Request.URL.Path, "status", resp.StatusCode)
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				slog.Error("host passthrough failed", "path", r.URL.Path, "err", err)
				w.WriteHeader(http.StatusBadGateway)
			},
		},
		cache: map[Identity]cachedToken{},
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Хост на пути из allowlist — в настоящие метаданные. Metadata-Flavor ответа ставят они: свой задвоил бы заголовок.
	// RequestURI сравнивается целиком: query и нестандартное экранирование пути не проходят.
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip != "" && ip == s.host.IP && r.Method == http.MethodGet && slices.Contains(s.host.Paths, r.RequestURI) {
		s.upstream.ServeHTTP(w, r)
		return
	}
	w.Header().Set("Metadata-Flavor", "Google")
	switch r.URL.Path {
	case "/healthz":
		io.WriteString(w, "ok")
	case tokenPath:
		s.token(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	// Как GCE metadata: без заголовка или через прокси (X-Forwarded-For) — отказ, защита от SSRF.
	if r.Header.Get("Metadata-Flavor") != "Google" || r.Header.Get("X-Forwarded-For") != "" {
		http.Error(w, "Metadata-Flavor: Google header required", http.StatusForbidden)
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	id, ok := s.resolver.Resolve(ip)
	if !ok {
		slog.Info("no identity for client", "ip", ip)
		http.NotFound(w, r)
		return
	}
	t, err := s.cachedToken(r.Context(), id)
	if err != nil {
		slog.Error("token exchange failed", "namespace", id.Namespace, "serviceAccount", id.ServiceAccount,
			"ycServiceAccountID", id.YCServiceAccountID, "err", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"access_token": t.token,
		"expires_in":   int64(time.Until(t.expiresAt).Seconds()),
		"token_type":   "Bearer",
	})
}

// cachedToken без дедупликации: параллельные промахи одного SA дают несколько обменов, все токены валидны.
func (s *Server) cachedToken(ctx context.Context, id Identity) (cachedToken, error) {
	s.mu.Lock()
	t, ok := s.cache[id]
	s.mu.Unlock()
	if ok && time.Until(t.expiresAt) > minValidity {
		return t, nil
	}
	token, expiresAt, err := s.exchanger.Token(ctx, id)
	if err != nil {
		return cachedToken{}, err
	}
	t = cachedToken{token, expiresAt}
	s.mu.Lock()
	s.cache[id] = t
	s.mu.Unlock()
	return t, nil
}
