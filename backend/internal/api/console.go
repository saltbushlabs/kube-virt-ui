package api

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"kvui/internal/auth"
	"kvui/internal/vm"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  32 << 10,
	WriteBufferSize: 32 << 10,
	// Checked by sameOrigin before upgrading.
	CheckOrigin: func(*http.Request) bool { return true },
}

// sameOrigin rejects cross-site websocket handshakes. Websockets are GETs, so
// the X-Requested-With CSRF check does not cover them.
func (s *Server) sameOrigin(r *http.Request) bool {
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || origin.Host == "" {
		return false
	}
	if origin.Host == r.Host {
		return true
	}
	public, err := url.Parse(s.PublicURL)
	return err == nil && origin.Host == public.Host
}

// console proxies the browser's websocket to KubeVirt's VNC or serial
// console stream for a VM instance, as the current user.
func (s *Server) console(w http.ResponseWriter, r *http.Request) {
	kind, name := chi.URLParam(r, "kind"), chi.URLParam(r, "name")
	if !vm.ConsoleKinds[kind] {
		writeError(w, http.StatusNotFound, "unknown console")
		return
	}
	if !s.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "cross-origin websocket refused")
		return
	}
	c, ok := s.clientsFor(w, r)
	if !ok {
		return
	}
	// Dial KubeVirt before upgrading so refusals (no permission, VM not
	// running) reach the browser as ordinary HTTP errors.
	upstream, err := vm.DialConsole(r.Context(), c.cfg, c.namespace, name, kind)
	if err != nil {
		kubeError(w, r, err)
		return
	}
	defer upstream.Close()
	client, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade has already written the error response
	}
	defer client.Close()

	u := currentUser(r)
	s.audit(r, u, "vm."+kind, c.namespace, name)
	cookie, _ := r.Cookie(sessionCookie)
	sessionHash := auth.HashToken(cookie.Value)

	errc := make(chan error, 2)
	go pump(upstream, client, errc)
	go pump(client, upstream, errc)

	ticker := time.NewTicker(s.consoleRecheck)
	defer ticker.Stop()
	for {
		select {
		case <-errc:
			msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "console closed")
			_ = client.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second)) // best effort; closing anyway
			return
		case <-ticker.C:
			if reason := s.consoleRevoked(sessionHash, u.ID, c.namespace, c.role); reason != "" {
				msg := websocket.FormatCloseMessage(websocket.ClosePolicyViolation, reason)
				_ = client.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second)) // best effort; closing anyway
				return
			}
			// Keeps idle consoles alive through proxies and load balancers.
			if err := client.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				return
			}
		}
	}
}

// consoleRevoked returns why an open console or VM watch stream must
// close, or "" if the session is still valid, the user still holds the
// role they opened it with, and the namespace is still a tenant. Any role
// change closes it, even operator <-> owner which both allow consoles: it
// is simpler than mirroring the ClusterRoles' console rules here, and the
// browser can reopen it under the new role.
func (s *Server) consoleRevoked(sessionHash string, userID int64, namespace, role string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u, err := s.Store.SessionUser(ctx, sessionHash, s.now())
	if err != nil || u.ID != userID || u.Disabled {
		return "session ended"
	}
	m, err := s.Store.Membership(ctx, userID, namespace)
	if err != nil || m.Role != role {
		return "access revoked"
	}
	// Fails closed like the checks above: an apiserver error also ends the
	// console.
	if err := s.Tenants.CheckTenant(ctx, namespace); err != nil {
		return "access revoked"
	}
	return ""
}

// pump copies messages from src to dst until either side fails.
func pump(dst, src *websocket.Conn, errc chan<- error) {
	for {
		mt, data, err := src.ReadMessage()
		if err == nil {
			err = dst.WriteMessage(mt, data)
		}
		if err != nil {
			errc <- err
			return
		}
	}
}
