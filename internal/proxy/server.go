package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	tokenPath = "/computeMetadata/v1/instance/service-accounts/default/token"
	// minValidity — токен из кэша отдаётся, только если до истечения больше этого.
	minValidity = 5 * time.Minute
)

// results — значения метки result у requests_total.
var results = []string{"cached", "exchanged", "forbidden", "no_identity", "exchange_error", "not_found"}

var (
	requests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "metadata_proxy_requests_total",
		Help: "Metadata requests by result; /healthz is not counted.",
	}, []string{"result"})
	exchangeDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "metadata_proxy_token_exchange_duration_seconds",
		Help: "Duration of k8s token to IAM token exchange, including failures.",
	})
	cacheEntries = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "metadata_proxy_token_cache_entries",
		Help: "Entries in the IAM token cache.",
	})
)

func init() {
	// Все result с нуля: rate() и алерты не ждут первого запроса каждого вида.
	for _, r := range results {
		requests.WithLabelValues(r)
	}
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// Server отвечает на token endpoint GCE metadata IAM-токеном SA пода-клиента.
type Server struct {
	resolver  *Resolver
	exchanger *Exchanger

	mu    sync.Mutex
	cache map[Identity]cachedToken
}

func NewServer(r *Resolver, e *Exchanger) *Server {
	return &Server{resolver: r, exchanger: e, cache: map[Identity]cachedToken{}}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Metadata-Flavor", "Google")
	switch r.URL.Path {
	case "/healthz":
		io.WriteString(w, "ok")
	case tokenPath:
		requests.WithLabelValues(s.token(w, r)).Inc()
	default:
		requests.WithLabelValues("not_found").Inc()
		http.NotFound(w, r)
	}
}

// token отвечает на запрос токена и возвращает result для requests_total.
func (s *Server) token(w http.ResponseWriter, r *http.Request) string {
	// Как GCE metadata: без заголовка или через прокси (X-Forwarded-For) — отказ, защита от SSRF.
	if r.Header.Get("Metadata-Flavor") != "Google" || r.Header.Get("X-Forwarded-For") != "" {
		http.Error(w, "Metadata-Flavor: Google header required", http.StatusForbidden)
		return "forbidden"
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	id, ok := s.resolver.Resolve(ip)
	if !ok {
		slog.Info("no identity for client", "ip", ip)
		http.NotFound(w, r)
		return "no_identity"
	}
	t, exchanged, err := s.cachedToken(r.Context(), id)
	if err != nil {
		slog.Error("token exchange failed", "namespace", id.Namespace, "serviceAccount", id.ServiceAccount,
			"ycServiceAccountID", id.YCServiceAccountID, "err", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return "exchange_error"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"access_token": t.token,
		"expires_in":   int64(time.Until(t.expiresAt).Seconds()),
		"token_type":   "Bearer",
	})
	if exchanged {
		return "exchanged"
	}
	return "cached"
}

// cachedToken без дедупликации: параллельные промахи одного SA дают несколько обменов, все токены валидны.
// bool — был ли обмен (промах кэша), в том числе неудачный.
func (s *Server) cachedToken(ctx context.Context, id Identity) (cachedToken, bool, error) {
	s.mu.Lock()
	t, ok := s.cache[id]
	s.mu.Unlock()
	if ok && time.Until(t.expiresAt) > minValidity {
		return t, false, nil
	}
	start := time.Now()
	token, expiresAt, err := s.exchanger.Token(ctx, id)
	exchangeDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		return cachedToken{}, true, err
	}
	t = cachedToken{token, expiresAt}
	s.mu.Lock()
	s.cache[id] = t
	cacheEntries.Set(float64(len(s.cache)))
	s.mu.Unlock()
	return t, true, nil
}
