package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kvui/internal/store"
)

func serveForTest(t *testing.T, srv *http.Server) (addr string, cancel context.CancelFunc, serveErr chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveErr = make(chan error, 1)
	go func() { serveErr <- serveHTTP(ctx, ln, srv) }()
	return ln.Addr().String(), cancel, serveErr
}

// A long-lived stream handler (the VM watch stream is one) must not hold
// shutdown up until the timeout. Streams watch a context that the
// RegisterOnShutdown hook cancels when shutdown begins.
func TestServeHTTPShutdownEndsStreams(t *testing.T) {
	old := shutdownTimeout
	shutdownTimeout = 5 * time.Second
	defer func() { shutdownTimeout = old }()

	streamCtx, closeStreams := context.WithCancel(context.Background())
	started := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-streamCtx.Done()
	})}
	srv.RegisterOnShutdown(closeStreams)
	addr, cancel, serveErr := serveForTest(t, srv)

	respErr := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/watch")
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		respErr <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never started")
	}

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serveHTTP: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown held up by the stream handler")
	}
	select {
	case <-respErr:
	case <-time.After(2 * time.Second):
		t.Fatal("stream request not ended by shutdown")
	}
}

// An ordinary in-flight request is not cancelled by shutdown: it keeps
// its request context and finishes while Shutdown drains.
func TestServeHTTPShutdownDrainsRequests(t *testing.T) {
	started := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(200 * time.Millisecond) // a write that must be allowed to finish
		if r.Context().Err() != nil {
			t.Error("request context cancelled by shutdown")
		}
		_, _ = w.Write([]byte("done"))
	})}
	addr, cancel, serveErr := serveForTest(t, srv)

	type result struct {
		body string
		err  error
	}
	res := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/create")
		if err != nil {
			res <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		res <- result{string(b), err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never started")
	}

	cancel() // shutdown begins while the request is in flight
	select {
	case r := <-res:
		if r.err != nil || r.body != "done" {
			t.Fatalf("in-flight request cut off by shutdown: %q %v", r.body, r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight request never completed")
	}
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serveHTTP: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serveHTTP did not return after draining")
	}
}

func TestLoadConfigPublicURL(t *testing.T) {
	validKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))

	setBase := func(t *testing.T, publicURL string) {
		t.Helper()
		t.Setenv("KVUI_SECRET_KEY", validKey)
		t.Setenv("KVUI_PUBLIC_URL", publicURL)
		t.Setenv("KVUI_INSECURE_COOKIES", "")
		t.Setenv("KVUI_TRUST_PROXY", "")
		t.Setenv("KVUI_TRUSTED_PROXIES", "")
	}

	for _, valid := range []string{
		"https://vms.example.com",
		"http://localhost:5173",
		"HTTPS://vms.example.com:8443/sub",
		"https://[::1]:8443",
	} {
		t.Run("valid/"+valid, func(t *testing.T) {
			setBase(t, valid)
			cfg, err := loadConfig()
			if err != nil {
				t.Fatalf("loadConfig(%q): unexpected error: %v", valid, err)
			}
			if cfg.publicURL != valid {
				t.Fatalf("publicURL = %q, want %q", cfg.publicURL, valid)
			}
		})
	}

	for _, invalid := range []string{
		"",
		"vms.example.com",
		"ftp://vms.example.com",
		"javascript:alert(1)",
		"https://",
		"https:///path",
		"https://:8080",
		"://bad",
	} {
		t.Run("invalid/"+invalid, func(t *testing.T) {
			setBase(t, invalid)
			if _, err := loadConfig(); err == nil {
				t.Fatalf("loadConfig(%q): expected error, got nil", invalid)
			}
		})
	}
}

func TestNewHTTPServerTimeouts(t *testing.T) {
	srv := newHTTPServer(":8080", http.NotFoundHandler())
	if srv.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", srv.Addr)
	}
	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 10s", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout != 120*time.Second {
		t.Errorf("IdleTimeout = %v, want 120s", srv.IdleTimeout)
	}
}

func TestCreateAdminInviteAudits(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	now := time.Unix(1_700_000_000, 0)
	link, err := createAdminInvite(ctx, st, "https://ui.test", now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, "https://ui.test/invite/") {
		t.Fatalf("unexpected invite link: %q", link)
	}
	invs, err := st.ListInvites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 1 || !invs[0].MakeAdmin {
		t.Fatalf("invites: %+v", invs)
	}
	entries, err := st.ListAudit(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %+v", entries)
	}
	got := entries[0]
	if got.ActorID != nil || got.Actor != "cli" || got.Action != "invite.create" || got.Namespace != "" || got.Target != "admin" || !got.At.Equal(now) {
		t.Fatalf("unexpected audit entry: %+v", got)
	}
}

func TestPurgeOnce(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	now := time.Unix(1_700_000_000, 0)
	inv, err := st.CreateInvite(ctx, "h1", store.Invite{MakeAdmin: true, CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.RedeemInviteNewUser(ctx, inv, store.User{Username: "alice", PasswordHash: "x", TOTPSecret: []byte("s")}, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, "expired", u.ID, now.Add(-2*time.Hour), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, "active", u.ID, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.Audit(ctx, store.AuditEntry{At: now.Add(-auditRetention - time.Second), Actor: "cli", Action: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Audit(ctx, store.AuditEntry{At: now.Add(-time.Hour), Actor: "cli", Action: "recent"}); err != nil {
		t.Fatal(err)
	}

	purgeOnce(ctx, st, now)

	if _, err := st.SessionUser(ctx, "expired", now.Add(-2*time.Minute)); err != store.ErrNotFound {
		t.Fatalf("expired session not purged: %v", err)
	}
	if _, err := st.SessionUser(ctx, "active", now); err != nil {
		t.Fatalf("active session purged: %v", err)
	}
	entries, err := st.ListAudit(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Action != "recent" {
		t.Fatalf("remaining audit entries: %+v", entries)
	}
}
