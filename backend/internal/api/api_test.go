package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"kvui/internal/auth"
	"kvui/internal/store"
	"kvui/internal/tenant"
	"kvui/internal/tenant/tenanttest"
)

type env struct {
	t     *testing.T
	srv   *httptest.Server
	k8s   *fake.Clientset
	store *store.Store
	clock time.Time
	// apiserver stands in for the KubeVirt API; requests holds what it saw.
	apiserver *httptest.Server
	requests  []string
	created   []byte // body of the last VM create the fake apiserver received
	api       *Server
}

func newEnv(t *testing.T) *env {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sealer, _ := auth.NewSealer(bytes.Repeat([]byte{1}, 32))
	k8s := tenanttest.FakeClient(tenanttest.Namespace("team-a", true), tenanttest.Namespace("kube-system", false))
	e := &env{t: t, k8s: k8s, store: st, clock: time.Now()}
	e.apiserver = httptest.NewServer(http.HandlerFunc(e.fakeKubeVirt))
	t.Cleanup(e.apiserver.Close)
	s := New(Options{Store: st, Tenants: tenant.NewProvisioner(k8s, &rest.Config{Host: e.apiserver.URL}), Kube: k8s, Sealer: sealer, PublicURL: "https://ui.test"})
	// Each call moves the clock one TOTP step so consecutive codes differ.
	s.now = func() time.Time { e.clock = e.clock.Add(30 * time.Second); return e.clock }
	e.api = s
	e.srv = httptest.NewServer(s.Handler())
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func (e *env) do(c *http.Client, method, path string, body any, out any) int {
	e.t.Helper()
	var r *bytes.Reader
	if body == nil {
		r = bytes.NewReader(nil)
	} else {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, r)
	req.Header.Set("X-Requested-With", "kvui")
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// code returns the TOTP code valid at the server's next clock tick.
func (e *env) code(secret string) string {
	c, err := auth.TOTPCode(secret, e.clock.Add(30*time.Second))
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

// redeem walks a new user through an invite link.
func (e *env) redeem(c *http.Client, link, username string) (secret string, recovery []string) {
	e.t.Helper()
	token := link[strings.LastIndex(link, "/")+1:]
	var totp struct{ Secret string }
	if st := e.do(c, "POST", "/api/invites/"+token+"/totp", map[string]string{"username": username}, &totp); st != 200 {
		e.t.Fatalf("totp: %d", st)
	}
	var res struct{ RecoveryCodes []string }
	st := e.do(c, "POST", "/api/invites/"+token+"/redeem",
		map[string]string{"username": username, "password": "a long enough password", "code": e.code(totp.Secret)}, &res)
	if st != 201 {
		e.t.Fatalf("redeem: %d", st)
	}
	return totp.Secret, res.RecoveryCodes
}

func TestInviteToNamespaceFlow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// Bootstrap admin via the CLI path.
	_, adminLink, err := CreateInvite(ctx, e.store, "https://ui.test",
		store.Invite{MakeAdmin: true, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	admin := e.client()
	e.redeem(admin, adminLink, "root")

	// Admin cannot invite into a non-tenant namespace.
	if st := e.do(admin, "POST", "/api/admin/invites", map[string]any{"namespace": "kube-system", "role": "owner"}, nil); st != 400 {
		t.Fatalf("kube-system invite: %d", st)
	}
	var created struct{ URL string }
	if st := e.do(admin, "POST", "/api/admin/invites", map[string]any{"namespace": "team-a", "role": "operator", "email": "a@x"}, &created); st != 201 {
		t.Fatalf("create invite: %d", st)
	}

	alice := e.client()
	secret, recovery := e.redeem(alice, created.URL, "alice")
	if len(recovery) != recoveryCodeCount {
		t.Fatalf("recovery codes: %v", recovery)
	}
	rb, err := e.k8s.RbacV1().RoleBindings("team-a").Get(ctx, "kvui-u2", metav1.GetOptions{})
	if err != nil || rb.RoleRef.Name != "kubevirt-ui-operator" {
		t.Fatalf("rolebinding: %v %v", rb, err)
	}

	// Reusing the link fails.
	token := created.URL[strings.LastIndex(created.URL, "/")+1:]
	if st := e.do(e.client(), "GET", "/api/invites/"+token, nil, nil); st != 410 {
		t.Fatalf("reused invite: %d", st)
	}

	// Non-admin is refused admin endpoints.
	if st := e.do(alice, "GET", "/api/admin/users", nil, nil); st != 403 {
		t.Fatalf("alice admin: %d", st)
	}

	// Fresh login: password + TOTP; then a replayed code fails; recovery code works once.
	fresh := e.client()
	login := map[string]string{"username": "alice", "password": "a long enough password", "code": e.code(secret)}
	before := e.clock
	if st := e.do(fresh, "POST", "/api/login", login, nil); st != 200 {
		t.Fatalf("login: %d", st)
	}
	e.clock = before // same TOTP step again: only replay protection can reject it
	if st := e.do(e.client(), "POST", "/api/login", login, nil); st != 401 {
		t.Fatalf("replayed code: %d", st)
	}
	rc := map[string]string{"username": "alice", "password": "a long enough password", "code": recovery[0]}
	if st := e.do(e.client(), "POST", "/api/login", rc, nil); st != 200 {
		t.Fatalf("recovery login: %d", st)
	}
	if st := e.do(e.client(), "POST", "/api/login", rc, nil); st != 401 {
		t.Fatalf("recovery reuse: %d", st)
	}

	var me struct{ Memberships []store.Membership }
	e.do(fresh, "GET", "/api/me", nil, &me)
	if len(me.Memberships) != 1 || me.Memberships[0].Namespace != "team-a" {
		t.Fatalf("me: %+v", me)
	}

	// Removing the membership deletes the ServiceAccount; disabling ends sessions.
	if st := e.do(admin, "DELETE", "/api/admin/users/2/memberships/team-a", nil, nil); st != 204 {
		t.Fatalf("remove membership: %d", st)
	}
	if _, err := e.k8s.CoreV1().ServiceAccounts("team-a").Get(ctx, "kvui-u2", metav1.GetOptions{}); err == nil {
		t.Fatal("serviceaccount survived membership removal")
	}
	if st := e.do(admin, "PATCH", "/api/admin/users/2", map[string]bool{"disabled": true}, nil); st != 204 {
		t.Fatalf("disable: %d", st)
	}
	if st := e.do(fresh, "GET", "/api/me", nil, nil); st != 401 {
		t.Fatalf("disabled user still has session: %d", st)
	}
}

// The TOTP step validates the username before any QR code is shown, and
// re-entering the step returns the same secret instead of rotating it.
func TestInviteTOTPValidatesAndReuses(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, adminLink, err := CreateInvite(ctx, e.store, "https://ui.test",
		store.Invite{MakeAdmin: true, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	e.redeem(e.client(), adminLink, "root")

	_, link, err := CreateInvite(ctx, e.store, "https://ui.test",
		store.Invite{Namespace: "team-a", Role: "viewer", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	token := link[strings.LastIndex(link, "/")+1:]
	c := e.client()
	totp := func(username string) (int, string) {
		var res struct{ Secret string }
		st := e.do(c, "POST", "/api/invites/"+token+"/totp", map[string]string{"username": username}, &res)
		return st, res.Secret
	}
	if st, _ := totp("NO WAY"); st != 400 {
		t.Fatalf("invalid username: %d", st)
	}
	if st, _ := totp("root"); st != 409 {
		t.Fatalf("taken username: %d", st)
	}
	st, first := totp("alice")
	if st != 200 || first == "" {
		t.Fatalf("totp: %d", st)
	}
	if st, again := totp("alice"); st != 200 || again != first {
		t.Fatalf("secret rotated on re-entry: %d", st)
	}
	// Without a usable invite there is no username-availability answer.
	var res struct{ Secret string }
	if st := e.do(c, "POST", "/api/invites/not-a-real-token/totp", map[string]string{"username": "root"}, &res); st != 404 {
		t.Fatalf("enumeration without invite: %d", st)
	}
}

func TestCSRFHeaderRequired(t *testing.T) {
	e := newEnv(t)
	resp, err := http.Post(e.srv.URL+"/api/login", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestLoginLockout(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	bad := map[string]string{"username": "nobody", "password": "wrong password here", "code": "000000"}
	for i := 0; i < 10; i++ {
		if st := e.do(c, "POST", "/api/login", bad, nil); st != 401 {
			t.Fatalf("attempt %d: %d", i, st)
		}
	}
	if st := e.do(c, "POST", "/api/login", bad, nil); st != 429 {
		t.Fatalf("not locked out: %d", st)
	}
}

// TestLoginLockoutIsPerSource checks that failures for an account from one
// source lock only that source out, not the account's owner elsewhere.
func TestLoginLockoutIsPerSource(t *testing.T) {
	e := newEnv(t)
	e.api.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	_, link, err := CreateInvite(context.Background(), e.store, "https://ui.test",
		store.Invite{MakeAdmin: true, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := e.redeem(e.client(), link, "root")

	login := func(from string, body map[string]string) int {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", e.srv.URL+"/api/login", bytes.NewReader(b))
		req.Header.Set("X-Requested-With", "kvui")
		req.Header.Set("X-Forwarded-For", from)
		resp, err := e.client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	const attacker, owner = "198.51.100.1", "203.0.113.7"
	bad := map[string]string{"username": "root", "password": "wrong password here", "code": "000000"}
	for i := 0; i < 10; i++ {
		if st := login(attacker, bad); st != 401 {
			t.Fatalf("attempt %d: %d", i, st)
		}
	}
	if st := login(attacker, bad); st != 429 {
		t.Fatalf("attacker not locked out: %d", st)
	}
	good := map[string]string{"username": "root", "password": "a long enough password", "code": e.code(secret)}
	if st := login(owner, good); st != 200 {
		t.Fatalf("owner locked out by another source: %d", st)
	}
}

func TestInviteTOTPConcurrentConsistentSecret(t *testing.T) {
	e := newEnv(t)
	e.api.now = time.Now
	ctx := context.Background()
	_, link, err := CreateInvite(ctx, e.store, "https://ui.test",
		store.Invite{Namespace: "team-a", Role: "viewer", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	token := link[strings.LastIndex(link, "/")+1:]

	const workers = 12
	start := make(chan struct{})
	secrets := make([]string, workers)
	statuses := make([]int, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/api/invites/"+token+"/totp", strings.NewReader(`{"username":"alice"}`))
			if err != nil {
				errs[idx] = err
				return
			}
			req.Header.Set("X-Requested-With", "kvui")
			resp, err := e.client().Do(req)
			if err != nil {
				errs[idx] = err
				return
			}
			defer resp.Body.Close()
			statuses[idx] = resp.StatusCode
			var res struct{ Secret string }
			errs[idx] = json.NewDecoder(resp.Body).Decode(&res)
			secrets[idx] = res.Secret
		}(i)
	}
	close(start)
	wg.Wait()

	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("worker %d error: %v", i, errs[i])
		}
		if statuses[i] != http.StatusOK || secrets[i] == "" {
			t.Fatalf("worker %d: status=%d secret=%q", i, statuses[i], secrets[i])
		}
		if secrets[i] != secrets[0] {
			t.Fatalf("worker %d got different secret %q != %q", i, secrets[i], secrets[0])
		}
	}

	// Redeeming with the returned secret succeeds.
	code, err := auth.TOTPCode(secrets[0], time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if st := e.do(e.client(), "POST", "/api/invites/"+token+"/redeem",
		map[string]string{"username": "alice", "password": "a long enough password", "code": code}, nil); st != http.StatusCreated {
		t.Fatalf("redeem with concurrent TOTP secret: %d", st)
	}
}

func TestInviteLimiterAcquireReleaseConcurrency(t *testing.T) {
	e := newEnv(t)
	e.api.now = time.Now
	e.api.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	e.api.inviteLimiter = auth.NewFailureLimiter(5, time.Minute)

	ctx := context.Background()
	_, link, err := CreateInvite(ctx, e.store, "https://ui.test",
		store.Invite{Namespace: "team-a", Role: "viewer", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	token := link[strings.LastIndex(link, "/")+1:]

	doFrom := func(from, method, path string, body any) int {
		t.Helper()
		var r *bytes.Reader
		if body == nil {
			r = bytes.NewReader(nil)
		} else {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, e.srv.URL+path, r)
		req.Header.Set("X-Requested-With", "kvui")
		req.Header.Set("X-Forwarded-For", from)
		resp, err := e.client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// Sequential valid calls release their slots and never lock themselves out.
	for i := range 8 {
		if st := doFrom("198.51.100.10", http.MethodGet, "/api/invites/"+token, nil); st != http.StatusOK {
			t.Fatalf("getInvite %d: %d", i, st)
		}
		if st := doFrom("198.51.100.10", http.MethodPost, "/api/invites/"+token+"/totp", map[string]string{"username": "alice"}); st != http.StatusOK {
			t.Fatalf("inviteTOTP %d: %d", i, st)
		}
	}

	// In-flight attempts acquired on the invite limiter block new requests until released.
	const inflightIP = "198.51.100.11"
	const inflightKey = "ip:" + inflightIP
	for range 5 {
		if !e.api.inviteLimiter.Acquire(inflightKey) {
			t.Fatal("Acquire failed unexpectedly")
		}
	}
	if st := doFrom(inflightIP, http.MethodGet, "/api/invites/"+token, nil); st != http.StatusTooManyRequests {
		t.Fatalf("while at inflight limit: got %d, want 429", st)
	}
	for range 5 {
		e.api.inviteLimiter.Release(inflightKey, false)
	}
	if st := doFrom(inflightIP, http.MethodGet, "/api/invites/"+token, nil); st != http.StatusOK {
		t.Fatalf("after releasing inflight slots: got %d, want 200", st)
	}

	// Concurrent burst of unknown-token probes cannot exceed the failure limit.
	const burstIP = "198.51.100.12"
	const burst = 15
	start := make(chan struct{})
	codes := make([]int, burst)
	burstErrs := make([]error, burst)
	var wg sync.WaitGroup
	for i := range burst {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			req, err := http.NewRequest(http.MethodGet, e.srv.URL+"/api/invites/nonexistent-token", nil)
			if err != nil {
				burstErrs[idx] = err
				return
			}
			req.Header.Set("X-Requested-With", "kvui")
			req.Header.Set("X-Forwarded-For", burstIP)
			resp, err := e.client().Do(req)
			if err != nil {
				burstErrs[idx] = err
				return
			}
			resp.Body.Close()
			codes[idx] = resp.StatusCode
		}(i)
	}
	close(start)
	wg.Wait()
	notFound, limited := 0, 0
	for i, st := range codes {
		if burstErrs[i] != nil {
			t.Fatalf("burst worker %d error: %v", i, burstErrs[i])
		}
		switch st {
		case http.StatusNotFound:
			notFound++
		case http.StatusTooManyRequests:
			limited++
		default:
			t.Fatalf("unexpected burst status: %d", st)
		}
	}
	if notFound > 5 || limited == 0 {
		t.Fatalf("burst bypassed limit: 404=%d 429=%d", notFound, limited)
	}

	// Wrong TOTP codes on redeemInvite record failures via Release and lock out.
	const redeemIP = "198.51.100.13"
	for i := range 5 {
		st := doFrom(redeemIP, http.MethodPost, "/api/invites/"+token+"/redeem",
			map[string]string{"username": "alice", "password": "a long enough password", "code": "000000"})
		if st != http.StatusBadRequest {
			t.Fatalf("bad totp attempt %d: got %d, want 400", i, st)
		}
	}
	if st := doFrom(redeemIP, http.MethodPost, "/api/invites/"+token+"/redeem",
		map[string]string{"username": "alice", "password": "a long enough password", "code": "000000"}); st != http.StatusTooManyRequests {
		t.Fatalf("redeem not locked out after 5 bad codes: got %d, want 429", st)
	}
}
