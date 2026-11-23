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
