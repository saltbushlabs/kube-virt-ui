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

type Provisioner struct {
	client kubernetes.Interface
	base   *rest.Config

	mu      sync.Mutex
	tokens  map[string]cachedToken // key: namespace/account
	ensured map[string]string      // key: namespace/account; role last fully applied by Ensure
}

type cachedToken struct {
	token   string
	expires time.Time
}

func NewProvisioner(client kubernetes.Interface, base *rest.Config) *Provisioner {
	return &Provisioner{client: client, base: base, tokens: map[string]cachedToken{}, ensured: map[string]string{}}
}

func (p *Provisioner) markEnsured(namespace string, userID int64, role string) {
	p.mu.Lock()
	p.ensured[namespace+"/"+accountName(userID)] = role
	p.mu.Unlock()
}

func (p *Provisioner) ensuredRole(namespace string, userID int64) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ensured[namespace+"/"+accountName(userID)]
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
	existing, err := rbs.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	case existing.RoleRef == want.RoleRef && len(existing.Subjects) == 1 && existing.Subjects[0] == want.Subjects[0]:
		p.markEnsured(namespace, userID, role)
		return nil
	default:
		// roleRef is immutable, so a role change is delete and recreate.
		if err := rbs.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete rolebinding: %w", err)
		}
	}
	if _, err := rbs.Create(ctx, want, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create rolebinding: %w", err)
	}
	p.markEnsured(namespace, userID, role)
	return nil
}

// Remove deletes a membership's RoleBinding and ServiceAccount. Deleting the
// ServiceAccount invalidates every token minted for it immediately.
func (p *Provisioner) Remove(ctx context.Context, namespace string, userID int64) error {
	name := accountName(userID)
	p.mu.Lock()
	delete(p.tokens, namespace+"/"+name)
	delete(p.ensured, namespace+"/"+name)
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
		p.mu.Lock()
		delete(p.ensured, namespace+"/"+accountName(userID))
		p.mu.Unlock()
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
	return cfg, nil
}

func (p *Provisioner) token(ctx context.Context, namespace string, userID int64) (string, error) {
	name := accountName(userID)
	key := namespace + "/" + name
	p.mu.Lock()
	c, ok := p.tokens[key]
	p.mu.Unlock()
	if ok && time.Until(c.expires) > tokenRefresh {
		return c.token, nil
	}
	ttl := int64(tokenTTL.Seconds())
	tr, err := p.client.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, name,
		&authv1.TokenRequest{Spec: authv1.TokenRequestSpec{ExpirationSeconds: &ttl}}, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.tokens[key] = cachedToken{token: tr.Status.Token, expires: tr.Status.ExpirationTimestamp.Time}
	p.mu.Unlock()
	return tr.Status.Token, nil
}
