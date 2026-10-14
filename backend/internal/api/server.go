// Package api is the HTTP JSON API consumed by the frontend.
package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"k8s.io/client-go/kubernetes"

	"kvui/internal/auth"
	"kvui/internal/image"
	"kvui/internal/store"
	"kvui/internal/tenant"
)

type Options struct {
	Store         *store.Store
	Tenants       *tenant.Provisioner
	Kube          kubernetes.Interface // the backend's own identity; cluster-scoped reads only
	UploadProxy   *image.UploadProxy   // nil disables image uploads
	Sealer        *auth.Sealer
	PublicURL     string // e.g. https://vms.example.com, used in invite links
	SecureCookies bool
	// TrustedProxies are the addresses of reverse proxies (the ingress
	// controller) whose X-Forwarded-For is believed. Empty: use the peer.
	TrustedProxies []netip.Prefix
	StaticDir      string // built frontend; empty disables static serving
}

type Server struct {
	Options
	loginLimiter  *auth.FailureLimiter
	inviteLimiter *auth.FailureLimiter
	now           func() time.Time
	// consoleRecheck is how often an open console or VM watch stream
	// re-validates the session and membership; removing access closes
	// them within this interval.
	consoleRecheck time.Duration
}

func New(o Options) *Server {
	return &Server{
		Options:       o,
		loginLimiter:  auth.NewFailureLimiter(10, 15*time.Minute),
		inviteLimiter: auth.NewFailureLimiter(20, 15*time.Minute),
		now:           time.Now,

		consoleRecheck: 30 * time.Second,
	}
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer, securityHeaders)
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	r.Route("/api", func(r chi.Router) {
		r.Use(requireCSRFHeader)
		r.Post("/login", s.login)
		r.Post("/logout", s.logout)
		r.Get("/invites/{token}", s.getInvite)
		r.Post("/invites/{token}/totp", s.inviteTOTP)
		r.Post("/invites/{token}/redeem", s.redeemInvite)

		r.Group(func(r chi.Router) {
			r.Use(s.requireUser)
			r.Get("/me", s.me)
			r.Post("/invites/{token}/accept", s.acceptInvite)
			r.Route("/namespaces/{ns}", func(r chi.Router) {
				r.Get("/access", s.access)
				r.Get("/vm-options", s.vmOptions)
				r.Get("/vms", s.listVMs)
				r.Post("/vms", s.createVM)
				r.Get("/watch/vms", s.watchVMs)
				r.Get("/vms/{name}", s.getVM)
				r.Delete("/vms/{name}", s.deleteVM)
				r.Post("/vms/{name}/{action}", s.vmAction)
				r.Get("/vms/{name}/{kind}", s.console)
				r.Get("/images", s.listImages)
				r.Post("/images", s.createImage)
				r.Delete("/images/{image}", s.deleteImage)
				r.Post("/images/{image}/upload", s.uploadImage)
			})

			r.Route("/admin", func(r chi.Router) {
				r.Use(requireAdmin)
				r.Get("/namespaces", s.adminNamespaces)
				r.Get("/invites", s.adminListInvites)
				r.Post("/invites", s.adminCreateInvite)
				r.Delete("/invites/{id}", s.adminRevokeInvite)
				r.Get("/users", s.adminListUsers)
				r.Patch("/users/{id}", s.adminUpdateUser)
				r.Put("/users/{id}/memberships/{ns}", s.adminPutMembership)
				r.Delete("/users/{id}/memberships/{ns}", s.adminDeleteMembership)
				r.Get("/audit", s.adminAudit)
			})
		})
	})

	if s.StaticDir != "" {
		r.NotFound(spa(s.StaticDir))
	}
	return r
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		// Invite tokens live in the URL path; never leak them via Referer.
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// requireCSRFHeader: a custom header cannot be set cross-origin without a
// CORS preflight, which we never grant. Together with SameSite=Strict
// cookies this blocks cross-site request forgery.
func requireCSRFHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if r.Header.Get("X-Requested-With") != "kvui" {
				writeError(w, http.StatusForbidden, "missing X-Requested-With header")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// spa serves the built frontend, falling back to index.html for client routes.
// Paths are resolved inside an fs.FS, which rejects anything escaping dir.
func spa(dir string) http.HandlerFunc {
	fsys := os.DirFS(dir)
	files := http.FileServerFS(fsys)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if info, err := fs.Stat(fsys, name); err != nil || info.IsDir() {
			http.ServeFileFS(w, r, fsys, "index.html")
			return
		}
		files.ServeHTTP(w, r)
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("write response", "err", err) // client went away
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// internalError logs err and returns a generic 500 so internals don't leak.
func internalError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("request failed", "method", r.Method, "route", routePattern(r), "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// routePattern returns the matched chi route (e.g. /api/invites/{token}).
// Logs use it instead of r.URL.Path because some paths carry bearer
// credentials such as invite tokens.
func routePattern(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil {
		if p := rc.RoutePattern(); p != "" {
			return p
		}
	}
	return "unknown"
}

// clientIP returns the address of the client that sent r. X-Forwarded-For
// is walked right to left only while the hop that reported it is a trusted
// proxy, so a client cannot choose its address by sending the header itself.
func (s *Server) clientIP(r *http.Request) netip.Addr {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	ip := peer.Addr().Unmap()
	var hops []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(h, ",")...)
	}
	for i := len(hops) - 1; i >= 0 && s.trustedProxy(ip); i-- {
		next, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		ip = next.WithZone("").Unmap()
	}
	return ip
}

func (s *Server) trustedProxy(ip netip.Addr) bool {
	for _, p := range s.TrustedProxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// rateKey is the rate-limiting key for r's client. IPv6 clients are keyed
// by /64, the smallest block an end site is normally assigned, so rotating
// addresses within it does not reset the limit.
func (s *Server) rateKey(r *http.Request) string {
	ip := s.clientIP(r)
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return "ip:" + p.String()
	}
	return "ip:" + ip.String()
}

func (s *Server) audit(r *http.Request, actor *store.User, action, namespace, target string) {
	e := store.AuditEntry{At: s.now(), Action: action, Namespace: namespace, Target: target}
	if ip := s.clientIP(r); ip.IsValid() {
		e.IP = ip.String()
	}
	if actor != nil {
		e.ActorID, e.Actor = &actor.ID, actor.Username
	}
	if err := s.Store.Audit(r.Context(), e); err != nil {
		slog.Error("audit write failed", "action", action, "err", err)
	}
}

// provision applies a membership to Kubernetes. Failures are logged rather
// than returned where the membership is already committed: UserConfig
// reprovisions on first use, so the user is not left stuck.
func (s *Server) provision(ctx context.Context, u store.User, namespace, role string) {
	if err := s.Tenants.Ensure(ctx, namespace, u.ID, u.Username, role); err != nil {
		slog.Error("provision membership", "user", u.Username, "namespace", namespace, "err", err)
	}
}
