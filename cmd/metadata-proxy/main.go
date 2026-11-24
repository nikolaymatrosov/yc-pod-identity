// metadata-proxy отвечает подам своей ноды на token endpoint GCE metadata IAM-токеном их YC SA через WIF.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/nikolaymatrosov/yc-pod-identity/internal/proxy"
)

func main() {
	nodeName := flag.String("node-name", "", "нода, подам которой выдаются токены (обязателен; в чарте $(NODE_NAME))")
	audience := flag.String("audience", "yc-pod-identity", "audience федерации для TokenRequest")
	listen := flag.String("listen", ":8080", "адрес HTTP-сервера")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if *nodeName == "" {
		slog.Error("-node-name is required")
		os.Exit(2)
	}
	if err := run(*nodeName, *audience, *listen); err != nil {
		slog.Error("exit", "err", err)
		os.Exit(1)
	}
}

func run(nodeName, audience, listen string) error {
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
	if err := resolver.Start(context.Background()); err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              listen,
		Handler:           proxy.NewServer(resolver, proxy.NewExchanger(client, audience), proxy.HostPassthrough{}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	slog.Info("listening", "addr", listen, "node", nodeName)
	return srv.ListenAndServe()
}
