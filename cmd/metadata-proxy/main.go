// metadata-proxy отвечает подам своей ноды на token endpoint GCE metadata IAM-токеном их YC SA через WIF.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/nikolaymatrosov/yc-pod-identity/internal/proxy"
)

func main() {
	nodeName := flag.String("node-name", "", "нода, подам которой выдаются токены (обязателен; в чарте $(NODE_NAME))")
	audience := flag.String("audience", "yc-pod-identity", "audience федерации для TokenRequest")
	listen := flag.String("listen", ":8080", "адрес HTTP-сервера")
	metricsListen := flag.String("metrics-listen", ":9090", "адрес сервера Prometheus-метрик (/metrics)")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if *nodeName == "" {
		slog.Error("-node-name is required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := run(ctx, *nodeName, *audience, *listen, *metricsListen); err != nil {
		slog.Error("exit", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, nodeName, audience, listen, metricsListen string) error {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	resolver := proxy.NewResolver(client, nodeName)
	// Порт открывается только после синка: до этого под не Ready и LRP на него не шлёт.
	if err := resolver.Start(ctx); err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              listen,
		Handler:           proxy.NewServer(resolver, proxy.NewExchanger(client, audience)),
		ReadHeaderTimeout: 5 * time.Second,
	}
	metrics := &http.Server{Addr: metricsListen, Handler: promhttp.Handler(), ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- srv.ListenAndServe() }()
	go func() { errc <- metrics.ListenAndServe() }()
	slog.Info("listening", "addr", listen, "metrics", metricsListen, "node", nodeName)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// Без паузы и переключения readiness: при раскатке без surge пауза только задержит новый под, а после снятия
	// бэкенда LRP запросы и так уходят в deny. Shutdown закрывает listener и ждёт текущие обмены.
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // как таймаут IAM-запроса обмена; в худшем случае Shutdown упрётся в дедлайн и выйдет с ошибкой
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
