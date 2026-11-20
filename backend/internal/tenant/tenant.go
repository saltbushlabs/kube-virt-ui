// Package tenant maps UI users onto Kubernetes RBAC. Each membership
// (user, namespace, role) becomes a ServiceAccount in that namespace bound
// to the kubevirt-ui-<role> ClusterRole. API calls made for a user use a
// short-lived token for that ServiceAccount, so the apiserver enforces what
// the user may do; the backend never decides authorization itself.
package tenant

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// TenantLabel marks namespaces that users may be invited into. The
	// backend refuses to provision anywhere else.
	TenantLabel = "kubevirt-ui.io/tenant"

	managedByLabel = "app.kubernetes.io/managed-by"
	managedBy      = "kubevirt-ui"
	userLabel      = "kubevirt-ui.io/user-id"
	usernameAnno   = "kubevirt-ui.io/username"

	tokenTTL     = 10 * time.Minute // TokenRequest minimum
	tokenRefresh = 2 * time.Minute

	maxRoleBindingAttempts = 3
)

var ErrNotTenant = errors.New("namespace is not a kubevirt-ui tenant namespace")

// Roles are the membership roles; each maps to a ClusterRole shipped by the
// Helm chart.
var Roles = []string{"owner", "operator", "viewer"}

func ValidRole(r string) bool {
	for _, v := range Roles {
		if v == r {
			return true
		}
	}
	return false
}

func ClusterRoleName(role string) string { return "kubevirt-ui-" + role }

func accountName(userID int64) string { return fmt.Sprintf("kvui-u%d", userID) }

func membershipKey(namespace string, userID int64) string {
	return namespace + "/" + accountName(userID)
}

type Provisioner struct {
	client kubernetes.Interface
	base   *rest.Config

	mu      sync.Mutex
	locks   map[string]*keyLock    // key: namespace/account; serializes Ensure/Remove/token
	tokens  map[string]cachedToken // key: namespace/account
	ensured map[string]string      // key: namespace/account; role last fully applied by Ensure
}

type keyLock struct {
	ch   chan struct{} // capacity 1; a value in ch means the lock is held
	refs int
	gen  uint64 // bumped on Remove and InvalidateToken to reject stale cache writes
}

type cachedToken struct {
	token   string
	expires time.Time
}

func NewProvisioner(client kubernetes.Interface, base *rest.Config) *Provisioner {
	return &Provisioner{
		client:  client,
		base:    base,
		locks:   map[string]*keyLock{},
		tokens:  map[string]cachedToken{},
		ensured: map[string]string{},
	}
}

// acquireKey serializes Ensure, Remove, and token calls for one
// (namespace, userID) key. When invalidate is true, cached token and ensured
// state are dropped and the key's generation is bumped before waiting on the
// lock so an in-flight call cannot write stale state back.
func (p *Provisioner) acquireKey(ctx context.Context, key string, invalidate bool) (*keyLock, func(), error) {
	p.mu.Lock()
	kl := p.locks[key]
	if kl == nil {
		kl = &keyLock{ch: make(chan struct{}, 1)}
		p.locks[key] = kl
	}
	if invalidate {
		delete(p.tokens, key)
		delete(p.ensured, key)
		kl.gen++
	}
	if err := ctx.Err(); err != nil {
		if kl.refs == 0 {
			delete(p.locks, key)
		}
		p.mu.Unlock()
		return nil, nil, err
	}
	kl.refs++
	p.mu.Unlock()

	select {
	case kl.ch <- struct{}{}:
		return kl, func() {
			p.mu.Lock()
			kl.refs--
			if kl.refs == 0 {
				delete(p.locks, key)
			}
			<-kl.ch
			p.mu.Unlock()
		}, nil
	case <-ctx.Done():
		p.mu.Lock()
		kl.refs--
		if kl.refs == 0 {
			delete(p.locks, key)
		}
		p.mu.Unlock()
		return nil, nil, ctx.Err()
	}
}

func (p *Provisioner) markEnsured(key string, kl *keyLock, gen uint64, role string) {
	p.mu.Lock()
	if kl.gen == gen {
		p.ensured[key] = role
	}
	p.mu.Unlock()
}

func (p *Provisioner) ensuredRole(namespace string, userID int64) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ensured[membershipKey(namespace, userID)]
}

// InvalidateToken drops any cached token and ensured state for a membership
// so the next UserConfig call re-verifies the ServiceAccount and RoleBinding
// and mints a fresh token.
func (p *Provisioner) InvalidateToken(namespace string, userID int64) {
	key := membershipKey(namespace, userID)
	p.mu.Lock()
	delete(p.tokens, key)
	delete(p.ensured, key)
	if kl := p.locks[key]; kl != nil {
		kl.gen++
	}
	p.mu.Unlock()
}

func (p *Provisioner) invalidateTokenIf(namespace string, userID int64, token string) {
	key := membershipKey(namespace, userID)
	p.mu.Lock()
	if c, ok := p.tokens[key]; ok && c.token == token {
		delete(p.tokens, key)
		delete(p.ensured, key)
		if kl := p.locks[key]; kl != nil {
			kl.gen++
		}
	}
	p.mu.Unlock()
}

// Namespaces lists tenant namespaces.
func (p *Provisioner) Namespaces(ctx context.Context) ([]string, error) {
	list, err := p.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: TenantLabel + "=true"})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list.Items))
	for _, ns := range list.Items {
		out = append(out, ns.Name)
	}
	sort.Strings(out)
	return out, nil
}

func (p *Provisioner) CheckTenant(ctx context.Context, namespace string) error {
	ns, err := p.client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ErrNotTenant
	}
	if err != nil {
		return err
	}
	if ns.Labels[TenantLabel] != "true" {
		return ErrNotTenant
	}
	return nil
}

func roleBindingMatches(existing, want *rbacv1.RoleBinding) bool {
	return existing.RoleRef == want.RoleRef &&
		len(existing.Subjects) == 1 &&
		existing.Subjects[0] == want.Subjects[0]
}

// Ensure makes the ServiceAccount and RoleBinding for a membership exist and
// match role. It is idempotent and safe to call on every membership change.
func (p *Provisioner) Ensure(ctx context.Context, namespace string, userID int64, username, role string) error {
	if !ValidRole(role) {
		return fmt.Errorf("invalid role %q", role)
	}
	if err := p.CheckTenant(ctx, namespace); err != nil {
		return err
	}
	name := accountName(userID)
	key := membershipKey(namespace, userID)
	kl, unlock, err := p.acquireKey(ctx, key, false)
	if err != nil {
		return err
	}
	defer unlock()

	p.mu.Lock()
	gen := kl.gen
	p.mu.Unlock()

	meta := metav1.ObjectMeta{
		Name:        name,
		Namespace:   namespace,
		Labels:      map[string]string{managedByLabel: managedBy, userLabel: fmt.Sprint(userID)},
		Annotations: map[string]string{usernameAnno: username},
	}

	sa := &corev1.ServiceAccount{ObjectMeta: meta, AutomountServiceAccountToken: new(bool)}
	if _, err := p.client.CoreV1().ServiceAccounts(namespace).Create(ctx, sa, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create serviceaccount: %w", err)
	}

	want := &rbacv1.RoleBinding{
		ObjectMeta: meta,
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: ClusterRoleName(role)},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: name, Namespace: namespace}},
	}
	rbs := p.client.RbacV1().RoleBindings(namespace)
	var createErr error
	for range maxRoleBindingAttempts {
		existing, err := rbs.Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return err
		case roleBindingMatches(existing, want):
			p.markEnsured(key, kl, gen, role)
			return nil
		default:
			// roleRef is immutable, so a role change is delete and recreate.
			// Drop ensured first so a failure between Delete and Create does
			// not leave a stale ensured role behind.
			p.mu.Lock()
			delete(p.ensured, key)
			p.mu.Unlock()
			if err := rbs.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete rolebinding: %w", err)
			}
		}
		_, err = rbs.Create(ctx, want, metav1.CreateOptions{})
		if err == nil {
			p.markEnsured(key, kl, gen, role)
			return nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create rolebinding: %w", err)
		}
		createErr = err
	}
	if existing, err := rbs.Get(ctx, name, metav1.GetOptions{}); err == nil && roleBindingMatches(existing, want) {
		p.markEnsured(key, kl, gen, role)
		return nil
	}
	return fmt.Errorf("create rolebinding: %w", createErr)
}

// Remove deletes a membership's RoleBinding and ServiceAccount. Deleting the
// ServiceAccount invalidates every token minted for it immediately.
func (p *Provisioner) Remove(ctx context.Context, namespace string, userID int64) error {
	name := accountName(userID)
	key := membershipKey(namespace, userID)
	kl, unlock, err := p.acquireKey(ctx, key, true)
	if err != nil {
		return err
	}
	defer unlock()

	// Clear again after acquiring the key lock in case a queued operation
	// ran between acquireKey's initial invalidation and lock handoff.
	p.mu.Lock()
	delete(p.tokens, key)
	delete(p.ensured, key)
	kl.gen++
	p.mu.Unlock()

	if err := p.client.RbacV1().RoleBindings(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err := p.client.CoreV1().ServiceAccounts(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// UserConfig returns a rest.Config that authenticates as the membership's
// ServiceAccount. Callers must have checked the membership exists. Unless
// this process has already applied role in full, Ensure runs first, so a
// partially provisioned membership (ServiceAccount without its RoleBinding,
// or a RoleBinding left at a previous role by a transient failure) heals on
// first use instead of only when the ServiceAccount itself is missing.
//
// The namespace must still be a tenant. Once an admin removes the tenant
// label, UserConfig returns ErrNotTenant and, best effort, removes the
// membership's ServiceAccount and RoleBinding so tokens already handed out
// stop working too (the admission policy keeps deletes allowed in
// de-labelled namespaces for this). The membership row is kept, so
// relabelling the namespace restores access: the ServiceAccount is then
// recreated as above.
func (p *Provisioner) UserConfig(ctx context.Context, namespace string, userID int64, username, role string) (*rest.Config, error) {
	if err := p.CheckTenant(ctx, namespace); err != nil {
		if errors.Is(err, ErrNotTenant) {
			// Remove drops the cached token before calling the apiserver, so
			// no further token is served even if the deletes fail.
			_ = p.Remove(ctx, namespace, userID) // best effort; access is denied regardless
		}
		return nil, err
	}
	if p.ensuredRole(namespace, userID) != role {
		if err := p.Ensure(ctx, namespace, userID, username, role); err != nil {
			return nil, err
		}
	}
	token, err := p.token(ctx, namespace, userID)
	if apierrors.IsNotFound(err) {
		// Deleted out of band after Ensure ran; recreate and retry once.
		p.InvalidateToken(namespace, userID)
		if err := p.Ensure(ctx, namespace, userID, username, role); err != nil {
			return nil, err
		}
		token, err = p.token(ctx, namespace, userID)
	}
	if err != nil {
		return nil, err
	}
	cfg := rest.AnonymousClientConfig(p.base)
	cfg.BearerToken = token
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return &unauthorizedInvalidatingTransport{
			base: rt,
			on401: func() {
				p.invalidateTokenIf(namespace, userID, token)
			},
		}
	})
	return cfg, nil
}

func (p *Provisioner) token(ctx context.Context, namespace string, userID int64) (string, error) {
	name := accountName(userID)
	key := membershipKey(namespace, userID)

	p.mu.Lock()
	if c, ok := p.tokens[key]; ok && time.Until(c.expires) > tokenRefresh {
		p.mu.Unlock()
		return c.token, nil
	}
	p.mu.Unlock()

	kl, unlock, err := p.acquireKey(ctx, key, false)
	if err != nil {
		return "", err
	}
	defer unlock()

	p.mu.Lock()
	if c, ok := p.tokens[key]; ok && time.Until(c.expires) > tokenRefresh {
		p.mu.Unlock()
		return c.token, nil
	}
	gen := kl.gen
	p.mu.Unlock()

	ttl := int64(tokenTTL.Seconds())
	tr, err := p.client.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, name,
		&authv1.TokenRequest{Spec: authv1.TokenRequestSpec{ExpirationSeconds: &ttl}}, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	if kl.gen == gen {
		p.tokens[key] = cachedToken{token: tr.Status.Token, expires: tr.Status.ExpirationTimestamp.Time}
	}
	p.mu.Unlock()
	return tr.Status.Token, nil
}

// unauthorizedInvalidatingTransport invalidates a cached ServiceAccount token
// when the apiserver rejects it with 401 Unauthorized (e.g. after an
// out-of-band ServiceAccount deletion) so the next request self-heals
// immediately instead of waiting for cache expiry.
type unauthorizedInvalidatingTransport struct {
	base  http.RoundTripper
	on401 func()
}

func (t *unauthorizedInvalidatingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil && resp != nil && resp.StatusCode == http.StatusUnauthorized {
		t.on401()
	}
	return resp, err
}

// WrappedRoundTripper satisfies k8s.io/apimachinery/pkg/util/net.RoundTripperWrapper
// so client-go can unwrap the underlying transport (for upgrades, idle
// connection closing, etc.).
func (t *unauthorizedInvalidatingTransport) WrappedRoundTripper() http.RoundTripper {
	return t.base
}
