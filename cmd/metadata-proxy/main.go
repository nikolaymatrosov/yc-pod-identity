// metadata-proxy отвечает подам своей ноды на token endpoint GCE metadata IAM-токеном их YC SA через WIF.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/nikolaymatrosov/yc-pod-identity/internal/proxy"
)

func main() {
	nodeName := flag.String("node-name", "", "нода, подам которой выдаются токены (обязателен; в чарте $(NODE_NAME))")
	audience := flag.String("audience", "yc-pod-identity", "audience федерации для TokenRequest")
	listen := flag.String("listen", ":8080", "адрес HTTP-сервера")
	hostPaths := flag.String("host-paths", "", "пути метаданных через запятую: запросы host namespace к ним уходят в "+
		"настоящие метаданные ВМ; пусто — выключено")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if *nodeName == "" {
		slog.Error("-node-name is required")
		os.Exit(2)
	}
	if err := run(*nodeName, *audience, *listen, hostPassthrough(*hostPaths)); err != nil {
		slog.Error("exit", "err", err)
		os.Exit(1)
	}
}

func run(nodeName, audience, listen string, host proxy.HostPassthrough) error {
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
		Handler:           proxy.NewServer(resolver, proxy.NewExchanger(client, audience), host),
		ReadHeaderTimeout: 5 * time.Second,
	}
	slog.Info("listening", "addr", listen, "node", nodeName, "hostIP", host.IP, "hostPaths", host.Paths)
	return srv.ListenAndServe()
}

// hostPassthrough собирает настройки пересылки запросов хоста. Без шлюза пересылка выключается, но прокси работает:
// упавший прокси увёл бы в deny токены всей ноды.
func hostPassthrough(paths string) proxy.HostPassthrough {
	host := proxy.HostPassthrough{Upstream: &url.URL{Scheme: "http", Host: "169.254.169.254"}}
	if paths == "" {
		return host
	}
	f, err := os.Open("/proc/net/route")
	if err == nil {
		host.IP, err = defaultGateway(f)
		f.Close()
	}
	if err != nil {
		slog.Warn("host passthrough disabled: no default gateway", "err", err)
		return host
	}
	host.Paths = strings.Split(paths, ",")
	return host
}

// defaultGateway возвращает шлюз маршрута по умолчанию из /proc/net/route. В поде Cilium это router IP — адрес
// cilium_host ноды, с которого после socket LB приходят connect() host namespace.
func defaultGateway(r io.Reader) (string, error) {
	s := bufio.NewScanner(r)
	for s.Scan() {
		f := strings.Fields(s.Text())
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		gw, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil {
			return "", fmt.Errorf("gateway %q: %w", f[2], err)
		}
		if gw == 0 {
			continue
		}
		return netip.AddrFrom4([4]byte{byte(gw), byte(gw >> 8), byte(gw >> 16), byte(gw >> 24)}).String(), nil
	}
	if err := s.Err(); err != nil {
		return "", err
	}
	return "", errors.New("no default route with a gateway")
}
