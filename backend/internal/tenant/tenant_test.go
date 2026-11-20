package tenant_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"kvui/internal/tenant"
	"kvui/internal/tenant/tenanttest"
)

func TestEnsureRoleChangeAndRemove(t *testing.T) {
	ctx := context.Background()
	c := tenanttest.FakeClient(tenanttest.Namespace("team-a", true))
	p := tenant.NewProvisioner(c, &rest.Config{Host: "https://k8s"})

	if err := p.Ensure(ctx, "team-a", 7, "alice", "viewer"); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(ctx, "team-a", 7, "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	rb, err := c.RbacV1().RoleBindings("team-a").Get(ctx, "kvui-u7", metav1.GetOptions{})
	if err != nil || rb.RoleRef.Name != "kubevirt-ui-owner" || rb.Subjects[0].Name != "kvui-u7" {
		t.Fatalf("rolebinding: %+v %v", rb, err)
	}

	cfg, err := p.UserConfig(ctx, "team-a", 7, "alice", "owner")
	if err != nil || cfg.BearerToken != "tok-team-a-kvui-u7" || cfg.Host != "https://k8s" {
		t.Fatalf("user config: %+v %v", cfg, err)
	}

	if err := p.Remove(ctx, "team-a", 7); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CoreV1().ServiceAccounts("team-a").Get(ctx, "kvui-u7", metav1.GetOptions{}); err == nil {
		t.Fatal("serviceaccount not removed")
	}
}

func TestRefusesNonTenantNamespace(t *testing.T) {
	ctx := context.Background()
	p := tenant.NewProvisioner(tenanttest.FakeClient(tenanttest.Namespace("kube-system", false)), &rest.Config{})
	if err := p.Ensure(ctx, "kube-system", 1, "x", "owner"); !errors.Is(err, tenant.ErrNotTenant) {
		t.Fatalf("kube-system: %v", err)
	}
	if err := p.Ensure(ctx, "missing", 1, "x", "owner"); !errors.Is(err, tenant.ErrNotTenant) {
		t.Fatalf("missing: %v", err)
	}
}

func TestUserConfigReprovisions(t *testing.T) {
	ctx := context.Background()
	c := tenanttest.FakeClient(tenanttest.Namespace("team-a", true))
	p := tenant.NewProvisioner(c, &rest.Config{})
	// No Ensure beforehand: UserConfig must create the ServiceAccount itself.
	if _, err := p.UserConfig(ctx, "team-a", 3, "bob", "viewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RbacV1().RoleBindings("team-a").Get(ctx, "kvui-u3", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
}

// A ServiceAccount left behind by an Ensure that failed before creating
// the RoleBinding must not strand the user: the token request succeeds, so
// UserConfig has to re-run Ensure based on the role it has applied, not on
// the ServiceAccount being missing.
func TestUserConfigHealsMissingRoleBinding(t *testing.T) {
	ctx := context.Background()
	c := tenanttest.FakeClient(tenanttest.Namespace("team-a", true))
	p := tenant.NewProvisioner(c, &rest.Config{})
	if _, err := c.CoreV1().ServiceAccounts("team-a").Create(ctx,
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "kvui-u3", Namespace: "team-a"}},
		metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UserConfig(ctx, "team-a", 3, "bob", "viewer"); err != nil {
		t.Fatal(err)
	}
	rb, err := c.RbacV1().RoleBindings("team-a").Get(ctx, "kvui-u3", metav1.GetOptions{})
	if err != nil || rb.RoleRef.Name != "kubevirt-ui-viewer" {
		t.Fatalf("rolebinding not healed: %+v %v", rb, err)
	}
}

// If the stored role changed but the matching Ensure failed transiently
// (e.g. in acceptInvite), the next UserConfig for the new role must apply
// it; conversely a RoleBinding upgraded before a failed store write (e.g.
// in adminPutMembership) must be put back to the stored role.
func TestUserConfigSyncsRoleDrift(t *testing.T) {
	ctx := context.Background()
	c := tenanttest.FakeClient(tenanttest.Namespace("team-a", true))
	p := tenant.NewProvisioner(c, &rest.Config{})
	if err := p.Ensure(ctx, "team-a", 3, "bob", "viewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UserConfig(ctx, "team-a", 3, "bob", "owner"); err != nil {
		t.Fatal(err)
	}
	rb, err := c.RbacV1().RoleBindings("team-a").Get(ctx, "kvui-u3", metav1.GetOptions{})
	if err != nil || rb.RoleRef.Name != "kubevirt-ui-owner" {
		t.Fatalf("role not upgraded: %+v %v", rb, err)
	}
	if _, err := p.UserConfig(ctx, "team-a", 3, "bob", "viewer"); err != nil {
		t.Fatal(err)
	}
	rb, err = c.RbacV1().RoleBindings("team-a").Get(ctx, "kvui-u3", metav1.GetOptions{})
	if err != nil || rb.RoleRef.Name != "kubevirt-ui-viewer" {
		t.Fatalf("role not restored: %+v %v", rb, err)
	}
}

func TestUserConfigRefusesDelabelledNamespace(t *testing.T) {
	ctx := context.Background()
	c := tenanttest.FakeClient(tenanttest.Namespace("team-a", true))
	p := tenant.NewProvisioner(c, &rest.Config{})
	if _, err := p.UserConfig(ctx, "team-a", 7, "alice", "operator"); err != nil {
		t.Fatal(err)
	}

	// An admin removes the tenant label: the cached token must not be served
	// and the membership's ServiceAccount and RoleBinding are cleaned up.
	if _, err := c.CoreV1().Namespaces().Update(ctx, tenanttest.Namespace("team-a", false), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UserConfig(ctx, "team-a", 7, "alice", "operator"); !errors.Is(err, tenant.ErrNotTenant) {
		t.Fatalf("de-labelled namespace: %v", err)
	}
	if _, err := c.CoreV1().ServiceAccounts("team-a").Get(ctx, "kvui-u7", metav1.GetOptions{}); err == nil {
		t.Fatal("serviceaccount not removed")
	}
	if _, err := c.RbacV1().RoleBindings("team-a").Get(ctx, "kvui-u7", metav1.GetOptions{}); err == nil {
		t.Fatal("rolebinding not removed")
	}

	// Relabelling restores access by reprovisioning.
	if _, err := c.CoreV1().Namespaces().Update(ctx, tenanttest.Namespace("team-a", true), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if cfg, err := p.UserConfig(ctx, "team-a", 7, "alice", "operator"); err != nil || cfg.BearerToken != "tok-team-a-kvui-u7" {
		t.Fatalf("relabelled: %+v %v", cfg, err)
	}

	// A namespace deleted outright is refused too.
	if _, err := p.UserConfig(ctx, "gone", 7, "alice", "operator"); !errors.Is(err, tenant.ErrNotTenant) {
		t.Fatalf("missing namespace: %v", err)
	}
}

func TestEnsureConcurrentFreshMembership(t *testing.T) {
	ctx := context.Background()
	c := tenanttest.FakeClient(tenanttest.Namespace("team-a", true))
	p := tenant.NewProvisioner(c, &rest.Config{Host: "https://k8s"})

	const workers = 16
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- p.Ensure(ctx, "team-a", 7, "alice", "operator")
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Ensure failed: %v", err)
		}
	}

	rb, err := c.RbacV1().RoleBindings("team-a").Get(ctx, "kvui-u7", metav1.GetOptions{})
	if err != nil || rb.RoleRef.Name != "kubevirt-ui-operator" {
		t.Fatalf("rolebinding: %+v %v", rb, err)
	}
}

func TestEnsureRoleBindingAlreadyExists(t *testing.T) {
	rbGVR := rbacv1.SchemeGroupVersion.WithResource("rolebindings")
	rbGR := schema.GroupResource{Group: rbacv1.GroupName, Resource: "rolebindings"}

	t.Run("matching", func(t *testing.T) {
		ctx := context.Background()
		c := tenanttest.FakeClient(tenanttest.Namespace("team-a", true))
		p := tenant.NewProvisioner(c, &rest.Config{})

		var injected atomic.Bool
		c.PrependReactor("create", "rolebindings", func(a k8stesting.Action) (bool, runtime.Object, error) {
			if !injected.CompareAndSwap(false, true) {
				return false, nil, nil
			}
			// Simulate another replica creating the matching RoleBinding first.
			want := a.(k8stesting.CreateAction).GetObject().(*rbacv1.RoleBinding).DeepCopy()
			if err := c.Tracker().Add(want); err != nil {
				return true, nil, err
			}
			return true, nil, apierrors.NewAlreadyExists(rbGR, want.Name)
		})

		if err := p.Ensure(ctx, "team-a", 7, "alice", "operator"); err != nil {
			t.Fatalf("Ensure failed on matching AlreadyExists: %v", err)
		}
		if !injected.Load() {
			t.Fatal("AlreadyExists was not injected")
		}
		rb, err := c.RbacV1().RoleBindings("team-a").Get(ctx, "kvui-u7", metav1.GetOptions{})
		if err != nil || rb.RoleRef.Name != "kubevirt-ui-operator" {
			t.Fatalf("rolebinding: %+v %v", rb, err)
		}
	})

	t.Run("mismatched", func(t *testing.T) {
		ctx := context.Background()
		c := tenanttest.FakeClient(tenanttest.Namespace("team-a", true))
		p := tenant.NewProvisioner(c, &rest.Config{})

		var injected atomic.Bool
		c.PrependReactor("create", "rolebindings", func(a k8stesting.Action) (bool, runtime.Object, error) {
			if !injected.CompareAndSwap(false, true) {
				return false, nil, nil
			}
			// Simulate another replica creating a RoleBinding for a different role.
			stale := &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "kvui-u7", Namespace: "team-a"},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: tenant.ClusterRoleName("viewer")},
				Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "kvui-u7", Namespace: "team-a"}},
			}
			if err := c.Tracker().Create(rbGVR, stale, "team-a"); err != nil {
				return true, nil, err
			}
			return true, nil, apierrors.NewAlreadyExists(rbGR, stale.Name)
		})

		if err := p.Ensure(ctx, "team-a", 7, "alice", "owner"); err != nil {
			t.Fatalf("Ensure failed on mismatched AlreadyExists: %v", err)
		}
		if !injected.Load() {
			t.Fatal("AlreadyExists was not injected")
		}
		rb, err := c.RbacV1().RoleBindings("team-a").Get(ctx, "kvui-u7", metav1.GetOptions{})
		if err != nil || rb.RoleRef.Name != "kubevirt-ui-owner" {
			t.Fatalf("rolebinding not replaced with owner: %+v %v", rb, err)
		}
	})
}

func TestEnsureConcurrentRoleUpdates(t *testing.T) {
	ctx := context.Background()
	c := tenanttest.FakeClient(tenanttest.Namespace("team-a", true))
	p := tenant.NewProvisioner(c, &rest.Config{})

	if err := p.Ensure(ctx, "team-a", 7, "alice", "viewer"); err != nil {
		t.Fatal(err)
	}

	// Track active RoleBinding mutations to assert per-key serialization:
	// two Ensure calls for the same key must never interleave Delete/Create.
	var active atomic.Int32
	var maxActive atomic.Int32
	record := func() func() {
		cur := active.Add(1)
		for {
			prev := maxActive.Load()
			if cur <= prev || maxActive.CompareAndSwap(prev, cur) {
				break
			}
		}
		return func() { active.Add(-1) }
	}
	c.PrependReactor("delete", "rolebindings", func(k8stesting.Action) (bool, runtime.Object, error) {
		defer record()()
		time.Sleep(time.Millisecond)
		return false, nil, nil
	})
	c.PrependReactor("create", "rolebindings", func(k8stesting.Action) (bool, runtime.Object, error) {
		defer record()()
		time.Sleep(time.Millisecond)
		return false, nil, nil
	})

	roles := []string{"viewer", "operator", "owner"}
	const workers = 12
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		role := roles[i%len(roles)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- p.Ensure(ctx, "team-a", 7, "alice", role)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent role update failed: %v", err)
		}
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("RoleBinding mutations overlapped: max concurrent = %d", got)
	}

	// Whichever role won last, UserConfig for every other role must detect
	// the difference and update the RoleBinding cleanly.
	rb, err := c.RbacV1().RoleBindings("team-a").Get(ctx, "kvui-u7", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get rolebinding: %v", err)
	}
	wantRole := "owner"
	if rb.RoleRef.Name == tenant.ClusterRoleName(wantRole) {
		wantRole = "viewer"
	}
	if _, err := p.UserConfig(ctx, "team-a", 7, "alice", wantRole); err != nil {
		t.Fatalf("UserConfig after concurrent updates: %v", err)
	}
	rb, err = c.RbacV1().RoleBindings("team-a").Get(ctx, "kvui-u7", metav1.GetOptions{})
	if err != nil || rb.RoleRef.Name != tenant.ClusterRoleName(wantRole) {
		t.Fatalf("rolebinding out of sync with ensured cache: got %+v, want %s", rb, wantRole)
	}
}

func TestTokenRacingWithRemoveDoesNotCacheStaleToken(t *testing.T) {
	t.Run("remove_while_token_in_flight", func(t *testing.T) {
		ctx := context.Background()
		c := tenanttest.FakeClient(tenanttest.Namespace("team-a", true))
		p := tenant.NewProvisioner(c, &rest.Config{Host: "https://k8s"})

		if err := p.Ensure(ctx, "team-a", 7, "alice", "operator"); err != nil {
			t.Fatal(err)
		}

		tokenInFlight := make(chan struct{})
		releaseToken := make(chan struct{})
		var mints atomic.Int32
		c.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
			if a.GetSubresource() != "token" {
				return false, nil, nil
			}
			name := a.(k8stesting.CreateActionImpl).Name
			if _, err := c.Tracker().Get(corev1.SchemeGroupVersion.WithResource("serviceaccounts"), a.GetNamespace(), name); err != nil {
				return true, nil, err
			}
			n := mints.Add(1)
			if n == 1 {
				close(tokenInFlight)
				<-releaseToken
			}
			return true, &authv1.TokenRequest{Status: authv1.TokenRequestStatus{
				Token:               fmt.Sprintf("tok-%d", n),
				ExpirationTimestamp: metav1.NewTime(time.Now().Add(10 * time.Minute)),
			}}, nil
		})

		cfgErr := make(chan error, 1)
		go func() {
			_, err := p.UserConfig(ctx, "team-a", 7, "alice", "operator")
			cfgErr <- err
		}()

		<-tokenInFlight

		// First, verify a cancelled Remove called mid-CreateToken still
		// bumps the key generation and stops the in-flight token from being
		// cached even without waiting for the key lock.
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if err := p.Remove(cancelled, "team-a", 7); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Remove: %v", err)
		}

		// Now start a live Remove concurrently with releasing the first CreateToken.
		removeErr := make(chan error, 1)
		go func() {
			removeErr <- p.Remove(ctx, "team-a", 7)
		}()
		close(releaseToken)

		if err := <-cfgErr; err != nil {
			t.Fatalf("first UserConfig: %v", err)
		}
		if err := <-removeErr; err != nil {
			t.Fatalf("Remove: %v", err)
		}

		// The ServiceAccount must be gone and the stale tok-1 must NOT be cached.
		if _, err := c.CoreV1().ServiceAccounts("team-a").Get(ctx, "kvui-u7", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatalf("serviceaccount survived Remove: %v", err)
		}
		cfg, err := p.UserConfig(ctx, "team-a", 7, "alice", "operator")
		if err != nil {
			t.Fatalf("second UserConfig: %v", err)
		}
		if cfg.BearerToken == "tok-1" {
			t.Fatalf("stale revoked token tok-1 was cached across Remove")
		}
		if cfg.BearerToken != "tok-2" {
			t.Fatalf("expected fresh tok-2, got %q", cfg.BearerToken)
		}
	})

	t.Run("cancelled_remove_prevents_caching", func(t *testing.T) {
		ctx := context.Background()
		c := tenanttest.FakeClient(tenanttest.Namespace("team-a", true))
		p := tenant.NewProvisioner(c, &rest.Config{Host: "https://k8s"})

		if err := p.Ensure(ctx, "team-a", 7, "alice", "operator"); err != nil {
			t.Fatal(err)
		}

		tokenInFlight := make(chan struct{})
		releaseToken := make(chan struct{})
		var mints atomic.Int32
		c.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
			if a.GetSubresource() != "token" {
				return false, nil, nil
			}
			n := mints.Add(1)
			if n == 1 {
				close(tokenInFlight)
				<-releaseToken
			}
			return true, &authv1.TokenRequest{Status: authv1.TokenRequestStatus{
				Token:               fmt.Sprintf("tok-%d", n),
				ExpirationTimestamp: metav1.NewTime(time.Now().Add(10 * time.Minute)),
			}}, nil
		})

		done := make(chan error, 1)
		go func() {
			_, err := p.UserConfig(ctx, "team-a", 7, "alice", "operator")
			done <- err
		}()
		<-tokenInFlight

		// Even if Remove's context is cancelled before acquiring the lock,
		// the generation bump must prevent the in-flight tok-1 from caching.
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_ = p.Remove(cancelled, "team-a", 7)

		close(releaseToken)
		if err := <-done; err != nil {
			t.Fatalf("first UserConfig: %v", err)
		}

		// Without calling Remove again, the next UserConfig must mint tok-2.
		cfg, err := p.UserConfig(ctx, "team-a", 7, "alice", "operator")
		if err != nil {
			t.Fatalf("second UserConfig: %v", err)
		}
		if cfg.BearerToken != "tok-2" {
			t.Fatalf("expected tok-2 after generation bump, got %q", cfg.BearerToken)
		}
	})
}

func TestUserConfigInvalidatesOn401AndSelfHeals(t *testing.T) {
	ctx := context.Background()
	c := tenanttest.FakeClient(tenanttest.Namespace("team-a", true))

	var mints atomic.Int32
	c.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "token" {
			return false, nil, nil
		}
		name := a.(k8stesting.CreateActionImpl).Name
		if _, err := c.Tracker().Get(corev1.SchemeGroupVersion.WithResource("serviceaccounts"), a.GetNamespace(), name); err != nil {
			return true, nil, err
		}
		n := mints.Add(1)
		return true, &authv1.TokenRequest{Status: authv1.TokenRequestStatus{
			Token:               fmt.Sprintf("tok-%d", n),
			ExpirationTimestamp: metav1.NewTime(time.Now().Add(10 * time.Minute)),
		}}, nil
	})

	var validToken atomic.Value
	validToken.Store("tok-1")
	apiserver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+validToken.Load().(string) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer apiserver.Close()

	p := tenant.NewProvisioner(c, &rest.Config{Host: apiserver.URL})

	cfg1, err := p.UserConfig(ctx, "team-a", 7, "alice", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if cfg1.BearerToken != "tok-1" {
		t.Fatalf("initial token: %q", cfg1.BearerToken)
	}

	// Verify WrappedRoundTripper is implemented so client-go unwrapping works.
	rt, err := rest.TransportFor(cfg1)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rt.(utilnet.RoundTripperWrapper); !ok {
		t.Fatalf("wrapped transport %T does not implement utilnet.RoundTripperWrapper", rt)
	}

	// Second call serves the cached token without minting again.
	cfgCached, err := p.UserConfig(ctx, "team-a", 7, "alice", "operator")
	if err != nil || cfgCached.BearerToken != "tok-1" || mints.Load() != 1 {
		t.Fatalf("expected cached tok-1, got %q (mints=%d, err=%v)", cfgCached.BearerToken, mints.Load(), err)
	}

	// Out-of-band deletion of the ServiceAccount invalidates tok-1 on the
	// apiserver. A request using a cloned config receives 401 and must
	// invalidate the cache immediately.
	if err := c.CoreV1().ServiceAccounts("team-a").Delete(ctx, "kvui-u7", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	validToken.Store("tok-2")

	cloned := rest.CopyConfig(cfg1)
	httpClient, err := rest.HTTPClientFor(cloned)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpClient.Get(apiserver.URL + "/api/v1/namespaces/team-a/pods")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with revoked token, got %d", resp.StatusCode)
	}

	// Next UserConfig must self-heal immediately: recreate SA/RB and mint tok-2.
	cfg2, err := p.UserConfig(ctx, "team-a", 7, "alice", "operator")
	if err != nil {
		t.Fatalf("self-healing UserConfig: %v", err)
	}
	if cfg2.BearerToken != "tok-2" {
		t.Fatalf("expected fresh tok-2 after 401 invalidation, got %q", cfg2.BearerToken)
	}
	if _, err := c.CoreV1().ServiceAccounts("team-a").Get(ctx, "kvui-u7", metav1.GetOptions{}); err != nil {
		t.Fatalf("serviceaccount not recreated: %v", err)
	}

	httpClient2, err := rest.HTTPClientFor(cfg2)
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := httpClient2.Get(apiserver.URL + "/api/v1/namespaces/team-a/pods")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with healed token, got %d", resp2.StatusCode)
	}

	// A delayed 401 on the old config (tok-1) must NOT evict the newer tok-2.
	respOld, err := httpClient.Get(apiserver.URL + "/api/v1/namespaces/team-a/pods")
	if err != nil {
		t.Fatal(err)
	}
	respOld.Body.Close()
	if respOld.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for stale client, got %d", respOld.StatusCode)
	}
	cfgStill2, err := p.UserConfig(ctx, "team-a", 7, "alice", "operator")
	if err != nil || cfgStill2.BearerToken != "tok-2" || mints.Load() != 2 {
		t.Fatalf("delayed 401 evicted fresh token: got %q (mints=%d, err=%v)", cfgStill2.BearerToken, mints.Load(), err)
	}

	// Explicit InvalidateToken drops the cache and forces re-provisioning/re-minting.
	p.InvalidateToken("team-a", 7)
	cfg3, err := p.UserConfig(ctx, "team-a", 7, "alice", "operator")
	if err != nil || cfg3.BearerToken != "tok-3" {
		t.Fatalf("expected tok-3 after InvalidateToken, got %q (err=%v)", cfg3.BearerToken, err)
	}
}
