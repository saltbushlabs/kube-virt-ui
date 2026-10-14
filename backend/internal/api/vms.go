package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	authzv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"kvui/internal/auth"
	"kvui/internal/store"
	"kvui/internal/tenant"
	"kvui/internal/vm"
)

const (
	// watchMaxDuration bounds each live-update stream. A watch keeps running
	// on the token it started with, so streams are cut periodically and the
	// browser reconnects, which re-checks the membership.
	watchMaxDuration = 5 * time.Minute
	watchDebounce    = 300 * time.Millisecond
	watchHeartbeat   = 25 * time.Second
)

// userClients are Kubernetes clients authenticated as the current user's
// ServiceAccount in one namespace.
type userClients struct {
	namespace, role string
	cfg             *rest.Config
	kube            kubernetes.Interface
	dyn             dynamic.Interface
}

// clientsFor checks the current user is a member of the {ns} URL namespace
// and returns clients acting as them there. It writes the error response
// itself on failure.
func (s *Server) clientsFor(w http.ResponseWriter, r *http.Request) (userClients, bool) {
	u, ns := currentUser(r), chi.URLParam(r, "ns")
	// UserConfig may (re)provision the ServiceAccount, so the membership
	// is read again afterwards: if it was removed in between, undo the
	// provisioning and refuse, rather than resurrecting a ServiceAccount
	// with no member behind it. A role changed in between is applied by
	// going round again, so the clients returned always match a
	// membership read taken after the last provisioning. The loop only
	// repeats while an admin keeps changing the role mid-request.
	var m store.Membership
	var cfg *rest.Config
	for {
		var err error
		m, err = s.Store.Membership(r.Context(), u.ID, ns)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusForbidden, "not a member of this namespace")
			return userClients{}, false
		}
		if err != nil {
			internalError(w, r, err)
			return userClients{}, false
		}
		cfg, err = s.Tenants.UserConfig(r.Context(), ns, u.ID, u.Username, m.Role)
		if errors.Is(err, tenant.ErrNotTenant) {
			writeError(w, http.StatusForbidden, "namespace is no longer a kubevirt-ui tenant")
			return userClients{}, false
		}
		if err != nil {
			internalError(w, r, err)
			return userClients{}, false
		}
		m2, err := s.Store.Membership(r.Context(), u.ID, ns)
		if errors.Is(err, store.ErrNotFound) {
			// Best effort, and not tied to this connection: a client that
			// has already gone away must not cancel the cleanup.
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
			_ = s.Tenants.Remove(cleanup, ns, u.ID)
			cancel()
			writeError(w, http.StatusForbidden, "not a member of this namespace")
			return userClients{}, false
		}
		if err != nil {
			internalError(w, r, err)
			return userClients{}, false
		}
		if m2.Role == m.Role {
			break
		}
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		internalError(w, r, err)
		return userClients{}, false
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		internalError(w, r, err)
		return userClients{}, false
	}
	return userClients{namespace: ns, role: m.Role, cfg: cfg, kube: kube, dyn: dyn}, true
}

// kubeError relays apiserver refusals (forbidden, not found, conflict) to
// the user and treats anything else as an internal error.
func kubeError(w http.ResponseWriter, r *http.Request, err error) {
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		if code := int(status.Status().Code); code >= 400 && code < 500 {
			writeError(w, code, status.Status().Message)
			return
		}
	}
	internalError(w, r, err)
}

// access returns what the current user may do in a namespace, as computed by
// the apiserver for their ServiceAccount.
func (s *Server) access(w http.ResponseWriter, r *http.Request) {
	c, ok := s.clientsFor(w, r)
	if !ok {
		return
	}
	review, err := c.kube.AuthorizationV1().SelfSubjectRulesReviews().Create(r.Context(),
		&authzv1.SelfSubjectRulesReview{Spec: authzv1.SelfSubjectRulesReviewSpec{Namespace: c.namespace}}, metav1.CreateOptions{})
	if err != nil {
		kubeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"role": c.role, "rules": review.Status.ResourceRules})
}

func (s *Server) listVMs(w http.ResponseWriter, r *http.Request) {
	c, ok := s.clientsFor(w, r)
	if !ok {
		return
	}
	vms, err := vm.List(r.Context(), c.dyn, c.namespace)
	if err != nil {
		kubeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, vms)
}

func (s *Server) getVM(w http.ResponseWriter, r *http.Request) {
	c, ok := s.clientsFor(w, r)
	if !ok {
		return
	}
	summary, err := vm.Get(r.Context(), c.dyn, c.namespace, chi.URLParam(r, "name"))
	if err != nil {
		kubeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) vmAction(w http.ResponseWriter, r *http.Request) {
	action := chi.URLParam(r, "action")
	if _, ok := vm.Actions[action]; !ok {
		writeError(w, http.StatusNotFound, "unknown action")
		return
	}
	c, ok := s.clientsFor(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")
	if err := vm.Do(r.Context(), c.kube.CoreV1().RESTClient(), c.namespace, name, action); err != nil {
		kubeError(w, r, err)
		return
	}
	s.audit(r, currentUser(r), "vm."+action, c.namespace, name)
	w.WriteHeader(http.StatusNoContent)
}

// watchVMs streams the namespace's VM list as server-sent events: the full
// list on connect and again (debounced) after every change. Like consoles,
// the stream re-validates the session and membership every consoleRecheck
// and closes when access ends (logout, disable, membership or tenant label
// removed) instead of serving VM data until the periodic cut: the token it
// lists with stays valid that whole time, and the apiserver does not end
// an already-open watch when its ServiceAccount is deleted.
func (s *Server) watchVMs(w http.ResponseWriter, r *http.Request) {
	c, ok := s.clientsFor(w, r)
	if !ok {
		return
	}
	u := currentUser(r)
	cookie, _ := r.Cookie(sessionCookie)
	sessionHash := auth.HashToken(cookie.Value)
	flusher, ok := w.(http.Flusher)
	if !ok {
		internalError(w, r, errors.New("streaming unsupported"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), watchMaxDuration)
	defer cancel()
	// End with the server's streams (shutdown) as well as with the request.
	defer context.AfterFunc(s.streamCtx, cancel)()
	changes, err := vm.Watch(ctx, c.dyn, c.namespace)
	if err != nil {
		kubeError(w, r, err)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // disable nginx ingress buffering
	fmt.Fprint(w, "retry: 2000\n\n")

	send := func() bool {
		vms, err := vm.List(ctx, c.dyn, c.namespace)
		if err != nil {
			if ctx.Err() == nil {
				fmt.Fprintf(w, "event: failure\ndata: %q\n\n", err.Error()) // #nosec G705 -- text/event-stream with nosniff, never rendered as HTML
				flusher.Flush()
			}
			return false
		}
		data, _ := json.Marshal(vms)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
		return true
	}
	if !send() {
		return
	}

	heartbeat := time.NewTicker(watchHeartbeat)
	defer heartbeat.Stop()
	recheck := time.NewTicker(s.consoleRecheck)
	defer recheck.Stop()
	debounce := time.NewTimer(0)
	<-debounce.C
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-changes:
			if !ok {
				return
			}
			debounce.Reset(watchDebounce)
		case <-debounce.C:
			if !send() {
				return
			}
		case <-recheck.C:
			if s.consoleRevoked(sessionHash, u.ID, c.namespace, c.role) != "" {
				return
			}
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

type storageClass struct {
	Name    string `json:"name"`
	Default bool   `json:"default"`
}

// vmOptions returns the choices for the create-VM form. Instancetypes and
// preferences are listed as the user; storage classes are cluster-scoped
// and not sensitive, so the backend lists them itself.
func (s *Server) vmOptions(w http.ResponseWriter, r *http.Request) {
	c, ok := s.clientsFor(w, r)
	if !ok {
		return
	}
	its, err := vm.ListInstancetypes(r.Context(), c.dyn, c.namespace)
	if err != nil {
		kubeError(w, r, err)
		return
	}
	prefs, err := vm.ListPreferences(r.Context(), c.dyn, c.namespace)
	if err != nil {
		kubeError(w, r, err)
		return
	}
	list, err := s.Kube.StorageV1().StorageClasses().List(r.Context(), metav1.ListOptions{})
	if err != nil {
		internalError(w, r, err)
		return
	}
	scs := make([]storageClass, 0, len(list.Items))
	for _, sc := range list.Items {
		scs = append(scs, storageClass{Name: sc.Name, Default: sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true"})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"instancetypes": nonNil(its), "preferences": nonNil(prefs), "storageClasses": scs,
	})
}

func (s *Server) createVM(w http.ResponseWriter, r *http.Request) {
	var req vm.CreateRequest
	if !decode(w, r, &req) {
		return
	}
	c, ok := s.clientsFor(w, r)
	if !ok {
		return
	}
	u := currentUser(r)
	err := vm.Create(r.Context(), c.dyn, c.namespace, u.Username, &req)
	if vm.IsValidation(err) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		kubeError(w, r, err)
		return
	}
	s.audit(r, u, "vm.create", c.namespace, req.Name)
	writeJSON(w, http.StatusCreated, map[string]string{"name": req.Name})
}

func (s *Server) deleteVM(w http.ResponseWriter, r *http.Request) {
	c, ok := s.clientsFor(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")
	if err := vm.Delete(r.Context(), c.dyn, c.namespace, name); err != nil {
		kubeError(w, r, err)
		return
	}
	s.audit(r, currentUser(r), "vm.delete", c.namespace, name)
	w.WriteHeader(http.StatusNoContent)
}
