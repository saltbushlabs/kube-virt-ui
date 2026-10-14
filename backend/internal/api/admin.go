package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"kvui/internal/store"
	"kvui/internal/tenant"
)

func (s *Server) adminNamespaces(w http.ResponseWriter, r *http.Request) {
	names, err := s.Tenants.Namespaces(r.Context())
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(names))
}

func (s *Server) adminListInvites(w http.ResponseWriter, r *http.Request) {
	invs, err := s.Store.ListInvites(r.Context())
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(invs))
}

func (s *Server) adminCreateInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email     string `json:"email"`
		Namespace string `json:"namespace"`
		Role      string `json:"role"`
		MakeAdmin bool   `json:"makeAdmin"`
		TTLHours  int    `json:"ttlHours"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Namespace == "" && !req.MakeAdmin {
		writeError(w, http.StatusBadRequest, "an invite needs a namespace, admin rights, or both")
		return
	}
	if req.Namespace != "" {
		if !tenant.ValidRole(req.Role) {
			writeError(w, http.StatusBadRequest, "role must be owner, operator or viewer")
			return
		}
		if err := s.Tenants.CheckTenant(r.Context(), req.Namespace); errors.Is(err, tenant.ErrNotTenant) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		} else if err != nil {
			internalError(w, r, err)
			return
		}
	} else {
		req.Role = ""
	}
	ttl := time.Duration(req.TTLHours) * time.Hour
	if ttl <= 0 {
		ttl = defaultInviteTTL
	}
	if ttl > maxInviteTTL {
		writeError(w, http.StatusBadRequest, "invites can last at most 168 hours")
		return
	}

	u, now := currentUser(r), s.now()
	inv, link, err := CreateInvite(r.Context(), s.Store, s.PublicURL, store.Invite{
		Email: req.Email, Namespace: req.Namespace, Role: req.Role, MakeAdmin: req.MakeAdmin,
		CreatedBy: &u.ID, CreatedAt: now, ExpiresAt: now.Add(ttl),
	})
	if err != nil {
		internalError(w, r, err)
		return
	}
	s.audit(r, u, "invite.create", inv.Namespace, req.Email)
	writeJSON(w, http.StatusCreated, map[string]any{"invite": inv, "url": link})
}

func (s *Server) adminRevokeInvite(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.Store.RevokeInvite(r.Context(), id, s.now()); errors.Is(err, store.ErrInviteUnusable) {
		writeError(w, http.StatusConflict, "invite already used or revoked")
		return
	} else if err != nil {
		internalError(w, r, err)
		return
	}
	s.audit(r, currentUser(r), "invite.revoke", "", strconv.FormatInt(id, 10))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		internalError(w, r, err)
		return
	}
	ms, err := s.Store.Memberships(r.Context(), 0)
	if err != nil {
		internalError(w, r, err)
		return
	}
	byUser := map[int64][]store.Membership{}
	for _, m := range ms {
		byUser[m.UserID] = append(byUser[m.UserID], m)
	}
	type userView struct {
		store.User
		Memberships []store.Membership `json:"memberships"`
	}
	out := make([]userView, len(users))
	for i, u := range users {
		out[i] = userView{u, nonNil(byUser[u.ID])}
	}
	writeJSON(w, http.StatusOK, out)
}

// targetUser loads the user named by the {id} URL parameter.
func (s *Server) targetUser(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return store.User{}, false
	}
	u, err := s.Store.UserByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "user not found")
		return store.User{}, false
	}
	if err != nil {
		internalError(w, r, err)
		return store.User{}, false
	}
	return u, true
}

func (s *Server) adminUpdateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Disabled bool `json:"disabled"`
	}
	if !decode(w, r, &req) {
		return
	}
	target, ok := s.targetUser(w, r)
	if !ok {
		return
	}
	if target.ID == currentUser(r).ID {
		writeError(w, http.StatusBadRequest, "you cannot disable yourself")
		return
	}
	if err := s.Store.SetDisabled(r.Context(), target.ID, req.Disabled); err != nil {
		internalError(w, r, err)
		return
	}
	action := "user.enable"
	if req.Disabled {
		action = "user.disable"
	}
	s.audit(r, currentUser(r), action, "", target.Username)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminPutMembership(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role string `json:"role"`
	}
	if !decode(w, r, &req) {
		return
	}
	if !tenant.ValidRole(req.Role) {
		writeError(w, http.StatusBadRequest, "role must be owner, operator or viewer")
		return
	}
	target, ok := s.targetUser(w, r)
	if !ok {
		return
	}
	ns := chi.URLParam(r, "ns")
	// Kubernetes first: it validates the namespace, and a leftover
	// ServiceAccount without a membership row grants nothing because the
	// backend only mints tokens for stored memberships.
	if err := s.Tenants.Ensure(r.Context(), ns, target.ID, target.Username, req.Role); errors.Is(err, tenant.ErrNotTenant) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	} else if err != nil {
		internalError(w, r, err)
		return
	}
	if err := s.Store.UpsertMembership(r.Context(), store.Membership{UserID: target.ID, Namespace: ns, Role: req.Role}, s.now()); err != nil {
		internalError(w, r, err)
		return
	}
	s.audit(r, currentUser(r), "membership.set:"+req.Role, ns, target.Username)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminDeleteMembership(w http.ResponseWriter, r *http.Request) {
	target, ok := s.targetUser(w, r)
	if !ok {
		return
	}
	ns := chi.URLParam(r, "ns")
	if _, err := s.Store.Membership(r.Context(), target.ID, ns); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "membership not found")
		return
	} else if err != nil {
		internalError(w, r, err)
		return
	}
	// Revoke in Kubernetes first so a failure leaves access intact and
	// visible rather than silently orphaned.
	if err := s.Tenants.Remove(r.Context(), ns, target.ID); err != nil {
		internalError(w, r, err)
		return
	}
	if err := s.Store.DeleteMembership(r.Context(), target.ID, ns); err != nil && !errors.Is(err, store.ErrNotFound) {
		internalError(w, r, err)
		return
	}
	// Remove again, best effort: a request from the removed user that
	// passed its membership check before the row was deleted re-creates
	// the ServiceAccount and RoleBinding in UserConfig. With the row now
	// gone, that request's own re-check refuses it, and this second
	// Remove deletes anything it recreated in the window, so no orphan
	// is left whichever way the timing falls. WithoutCancel because the
	// cleanup must not die with the admin's connection.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	defer cancel()
	if err := s.Tenants.Remove(cleanup, ns, target.ID); err != nil {
		slog.Error("cleanup after membership removal", "namespace", ns, "user", target.Username, "err", err)
	}
	s.audit(r, currentUser(r), "membership.remove", ns, target.Username)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminAudit(w http.ResponseWriter, r *http.Request) {
	entries, err := s.Store.ListAudit(r.Context(), 500)
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(entries))
}
