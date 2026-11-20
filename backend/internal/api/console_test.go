package api

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"kvui/internal/auth"
	"kvui/internal/store"
	"kvui/internal/tenant/tenanttest"
)

// setTenant labels or de-labels a namespace in the fake cluster.
func (e *env) setTenant(namespace string, isTenant bool) {
	e.t.Helper()
	if _, err := e.k8s.CoreV1().Namespaces().Update(context.Background(),
		tenanttest.Namespace(namespace, isTenant), metav1.UpdateOptions{}); err != nil {
		e.t.Fatal(err)
	}
}

func TestConsoleRevoked(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, link, _ := CreateInvite(ctx, e.store, "https://ui.test", store.Invite{
		Namespace: "team-a", Role: "operator", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	alice := e.client()
	e.redeem(alice, link, "alice")
	srvURL, _ := url.Parse(e.srv.URL)
	var sessionHash string
	for _, c := range alice.Jar.Cookies(srvURL) {
		if c.Name == sessionCookie {
			sessionHash = auth.HashToken(c.Value)
		}
	}
	if sessionHash == "" {
		t.Fatal("no session cookie")
	}

	if reason := e.api.consoleRevoked(sessionHash, 1, "team-a", "operator"); reason != "" {
		t.Fatalf("valid console revoked: %q", reason)
	}

	// Downgrading to viewer (no console access) closes the console.
	if err := e.store.UpsertMembership(ctx, store.Membership{UserID: 1, Namespace: "team-a", Role: "viewer"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if reason := e.api.consoleRevoked(sessionHash, 1, "team-a", "operator"); reason == "" {
		t.Fatal("console kept open after downgrade to viewer")
	}
	// So does any other role change.
	if err := e.store.UpsertMembership(ctx, store.Membership{UserID: 1, Namespace: "team-a", Role: "owner"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if reason := e.api.consoleRevoked(sessionHash, 1, "team-a", "operator"); reason == "" {
		t.Fatal("console kept open after role change")
	}

	// Removing the tenant label closes the console.
	if err := e.store.UpsertMembership(ctx, store.Membership{UserID: 1, Namespace: "team-a", Role: "operator"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	e.setTenant("team-a", false)
	if reason := e.api.consoleRevoked(sessionHash, 1, "team-a", "operator"); reason == "" {
		t.Fatal("console kept open after namespace lost tenant label")
	}
}

// fakeConsole stands in for KubeVirt's console websocket: it requires the
// plain.kubevirt.io subprotocol and echoes every message back.
func fakeConsole(w http.ResponseWriter, r *http.Request) {
	if !slices.Contains(websocket.Subprotocols(r), "plain.kubevirt.io") {
		http.Error(w, "missing subprotocol", http.StatusBadRequest)
		return
	}
	up := websocket.Upgrader{Subprotocols: []string{"plain.kubevirt.io"}}
	c, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close()
	for {
		mt, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		c.WriteMessage(mt, data)
	}
}

func TestConsoleProxy(t *testing.T) {
	e := newEnv(t)
	e.api.consoleRecheck = 50 * time.Millisecond
	ctx := context.Background()
	_, link, _ := CreateInvite(ctx, e.store, "https://ui.test", store.Invite{
		Namespace: "team-a", Role: "operator", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	alice := e.client()
	e.redeem(alice, link, "alice")

	base := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/api/namespaces/team-a/vms/"
	dialer := websocket.Dialer{Jar: alice.Jar}
	sameOrigin := http.Header{"Origin": {e.srv.URL}}

	if _, resp, err := dialer.Dial(base+"web/vnc", http.Header{"Origin": {"https://evil.test"}}); err == nil || resp.StatusCode != 403 {
		t.Fatalf("cross-origin handshake accepted: %v", err)
	}
	if _, resp, err := dialer.Dial(base+"web/vnc", http.Header{"Origin": {"http://ui.test"}}); err == nil || resp.StatusCode != 403 {
		t.Fatalf("scheme-mismatched PublicURL handshake accepted: %v", err)
	}
	if _, resp, err := dialer.Dial(base+"web/vnc", http.Header{"Origin": {"https://" + strings.TrimPrefix(e.srv.URL, "http://")}}); err == nil || resp.StatusCode != 403 {
		t.Fatalf("scheme-mismatched host handshake accepted: %v", err)
	}
	if _, resp, err := dialer.Dial(base+"off/vnc", sameOrigin); err == nil || resp.StatusCode != 404 {
		t.Fatalf("upstream refusal not relayed: %v", err)
	}

	conn, _, err := dialer.Dial(base+"web/vnc", sameOrigin)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("RFB 003.008\n")); err != nil {
		t.Fatal(err)
	}
	if mt, data, err := conn.ReadMessage(); err != nil || mt != websocket.BinaryMessage || string(data) != "RFB 003.008\n" {
		t.Fatalf("echo: %d %q %v", mt, data, err)
	}
	if last := e.requests[len(e.requests)-1]; !strings.HasSuffix(last, "Bearer tok-team-a-kvui-u1") {
		t.Fatalf("upstream not dialled as alice: %s", last)
	}

	// Removing the membership closes the open console.
	if err := e.store.DeleteMembership(ctx, 1, "team-a"); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = conn.ReadMessage()
	if !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("console not closed on revocation: %v", err)
	}
}

func TestSameOrigin(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	cases := []struct {
		name           string
		publicURL      string
		secureCookies  bool
		trustedProxies []netip.Prefix
		remoteAddr     string
		forwardedProto string
		tls            bool
		host           string
		origin         string
		want           bool
	}{
		{
			name:          "public url exact match",
			publicURL:     "https://ui.test",
			secureCookies: true,
			host:          "kvui.internal:8080",
			origin:        "https://ui.test",
			want:          true,
		},
		{
			name:          "public url case-insensitive scheme and host",
			publicURL:     "https://ui.test",
			secureCookies: true,
			host:          "kvui.internal:8080",
			origin:        "HTTPS://UI.TEST",
			want:          true,
		},
		{
			name:          "public url scheme mismatch http vs https",
			publicURL:     "https://ui.test",
			secureCookies: false,
			host:          "kvui.internal:8080",
			origin:        "http://ui.test",
			want:          false,
		},
		{
			name:          "public url scheme mismatch rejected even when matching host",
			publicURL:     "https://ui.test",
			secureCookies: true,
			host:          "ui.test",
			origin:        "http://ui.test",
			want:          false,
		},
		{
			name:          "kubectl port-forward over plain http works with secure cookies",
			publicURL:     "https://ui.test",
			secureCookies: true,
			host:          "localhost:8080",
			origin:        "http://localhost:8080",
			want:          true,
		},
		{
			name:           "trusted proxy forwarded https accepts https host",
			publicURL:      "https://other.test",
			secureCookies:  true,
			trustedProxies: trusted,
			remoteAddr:     "10.0.0.5:43210",
			forwardedProto: "https",
			host:           "ui.test",
			origin:         "https://ui.test",
			want:           true,
		},
		{
			name:           "trusted proxy forwarded https rejects http host",
			publicURL:      "https://other.test",
			secureCookies:  true,
			trustedProxies: trusted,
			remoteAddr:     "10.0.0.5:43210",
			forwardedProto: "https",
			host:           "ui.test",
			origin:         "http://ui.test",
			want:           false,
		},
		{
			name:           "untrusted proxy forwarded https is ignored",
			publicURL:      "https://other.test",
			secureCookies:  true,
			trustedProxies: trusted,
			remoteAddr:     "198.51.100.5:43210",
			forwardedProto: "https",
			host:           "ui.test",
			origin:         "https://ui.test",
			want:           false,
		},
		{
			name:          "insecure plain http host match",
			publicURL:     "https://ui.test",
			secureCookies: false,
			host:          "localhost:8080",
			origin:        "http://localhost:8080",
			want:          true,
		},
		{
			name:          "insecure plain http rejects https host mismatch",
			publicURL:     "https://ui.test",
			secureCookies: false,
			host:          "localhost:8080",
			origin:        "https://localhost:8080",
			want:          false,
		},
		{
			name:          "direct tls accepts https host",
			publicURL:     "https://ui.test",
			secureCookies: false,
			tls:           true,
			host:          "localhost:8443",
			origin:        "https://localhost:8443",
			want:          true,
		},
		{
			name:          "direct tls rejects http host",
			publicURL:     "https://ui.test",
			secureCookies: false,
			tls:           true,
			host:          "localhost:8443",
			origin:        "http://localhost:8443",
			want:          false,
		},
		{
			name:          "missing origin rejected",
			publicURL:     "https://ui.test",
			secureCookies: true,
			host:          "ui.test",
			origin:        "",
			want:          false,
		},
		{
			name:          "schemeless origin rejected",
			publicURL:     "https://ui.test",
			secureCookies: false,
			host:          "ui.test",
			origin:        "//ui.test",
			want:          false,
		},
		{
			name:          "ws scheme rejected",
			publicURL:     "https://ui.test",
			secureCookies: false,
			host:          "localhost:8080",
			origin:        "ws://localhost:8080",
			want:          false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Options{PublicURL: tc.publicURL, SecureCookies: tc.secureCookies, TrustedProxies: tc.trustedProxies})
			r := httptest.NewRequest(http.MethodGet, "/api/namespaces/team-a/vms/web/vnc", nil)
			r.Host = tc.host
			if tc.remoteAddr != "" {
				r.RemoteAddr = tc.remoteAddr
			}
			if tc.forwardedProto != "" {
				r.Header.Set("X-Forwarded-Proto", tc.forwardedProto)
			}
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if got := s.sameOrigin(r); got != tc.want {
				t.Errorf("sameOrigin() = %v, want %v", got, tc.want)
			}
		})
	}
}
