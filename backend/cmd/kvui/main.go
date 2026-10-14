// Command kvui runs the KubeVirt UI backend.
//
//	kvui                 serve the API and frontend
//	kvui admin-invite    print a one-time link that creates an admin account
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"kvui/internal/api"
	"kvui/internal/auth"
	"kvui/internal/image"
	"kvui/internal/store"
	"kvui/internal/tenant"
)

type config struct {
	listen         string
	dbPath         string
	secretKey      []byte
	publicURL      string
	staticDir      string
	insecure       bool // plain-HTTP cookies, for local development only
	trustedProxies []netip.Prefix

	cdiNamespace      string
	uploadProxyURL    string
	uploadProxyServer string // TLS name to verify, when the URL's host differs (e.g. a port-forward)
}

func loadConfig() (config, error) {
	c := config{
		listen:    env("KVUI_LISTEN", ":8080"),
		dbPath:    env("KVUI_DB", "kvui.db"),
		publicURL: os.Getenv("KVUI_PUBLIC_URL"),
		staticDir: os.Getenv("KVUI_STATIC_DIR"),

		cdiNamespace:      env("KVUI_CDI_NAMESPACE", "cdi"),
		uploadProxyServer: os.Getenv("KVUI_CDI_UPLOAD_SERVER_NAME"),
	}
	c.uploadProxyURL = env("KVUI_CDI_UPLOAD_URL", "https://cdi-uploadproxy."+c.cdiNamespace+".svc")
	var err error
	if c.insecure, err = envBool("KVUI_INSECURE_COOKIES"); err != nil {
		return c, err
	}
	if os.Getenv("KVUI_TRUST_PROXY") != "" {
		return c, errors.New("KVUI_TRUST_PROXY was replaced by KVUI_TRUSTED_PROXIES: set it to the ingress controller's address range(s), e.g. 10.244.0.0/16")
	}
	for _, v := range strings.Split(os.Getenv("KVUI_TRUSTED_PROXIES"), ",") {
		if v = strings.TrimSpace(v); v == "" {
			continue
		}
		p, err := netip.ParsePrefix(v)
		if err != nil {
			return c, fmt.Errorf("KVUI_TRUSTED_PROXIES: %w", err)
		}
		c.trustedProxies = append(c.trustedProxies, p.Masked())
	}
	if c.publicURL == "" {
		return c, errors.New("KVUI_PUBLIC_URL is required (used in invite links)")
	}
	c.secretKey, err = base64.StdEncoding.DecodeString(os.Getenv("KVUI_SECRET_KEY"))
	if err != nil || len(c.secretKey) != 32 {
		return c, errors.New("KVUI_SECRET_KEY must be 32 bytes, base64 encoded (openssl rand -base64 32)")
	}
	return c, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

// kubeConfig uses the in-cluster ServiceAccount, falling back to the
// developer's kubeconfig when run outside the cluster.
func kubeConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, nil).ClientConfig()
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "admin-invite":
			now := time.Now()
			_, link, err := api.CreateInvite(context.Background(), st, cfg.publicURL,
				store.Invite{MakeAdmin: true, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)})
			if err != nil {
				return err
			}
			fmt.Println("Admin invite (valid 24h, single use):")
			fmt.Println(link)
			return nil
		default:
			return fmt.Errorf("unknown command %q", os.Args[1])
		}
	}

	sealer, err := auth.NewSealer(cfg.secretKey)
	if err != nil {
		return err
	}
	restCfg, err := kubeConfig()
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return err
	}

	srv := api.New(api.Options{
		Store:   st,
		Tenants: tenant.NewProvisioner(client, restCfg),
		Kube:    client,
		UploadProxy: &image.UploadProxy{
			URL:                  cfg.uploadProxyURL,
			ServerName:           cfg.uploadProxyServer,
			CAConfigMapNamespace: cfg.cdiNamespace,
			CAConfigMapName:      "cdi-uploadproxy-signer-bundle",
			CAConfigMapKey:       "ca-bundle.crt",
			Kube:                 client,
		},
		Sealer:         sealer,
		PublicURL:      cfg.publicURL,
		SecureCookies:  !cfg.insecure,
		TrustedProxies: cfg.trustedProxies,
		StaticDir:      cfg.staticDir,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go purgeSessions(ctx, st)

	httpSrv := &http.Server{Addr: cfg.listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return err
	}
	slog.Info("listening", "addr", cfg.listen)
	return serveHTTP(ctx, ln, httpSrv)
}

var shutdownTimeout = 10 * time.Second

// serveHTTP serves on ln until ctx ends, then shuts down gracefully and
// waits for that to finish. Request contexts derive from ctx (BaseContext),
// so cancellation ends long-lived handlers such as the VM watch streams at
// once: without it Shutdown would wait on them until the timeout, and
// without waiting for Shutdown here the process would exit mid-request as
// soon as the listener closed.
func serveHTTP(ctx context.Context, ln net.Listener, srv *http.Server) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	srv.BaseContext = func(net.Listener) context.Context { return ctx }
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			slog.Error("shutdown", "err", err)
		}
	}()
	err := srv.Serve(ln)
	cancel() // if Serve failed on its own, release the shutdown goroutine
	<-done
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func purgeSessions(ctx context.Context, st *store.Store) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := st.PurgeExpiredSessions(ctx, time.Now()); err != nil {
				slog.Error("purge sessions", "err", err)
			}
		}
	}
}
