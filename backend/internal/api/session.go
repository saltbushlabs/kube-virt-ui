package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"kvui/internal/auth"
	"kvui/internal/store"
)

const (
	sessionCookie = "kvui_session"
	sessionTTL    = 12 * time.Hour
)

type ctxKey struct{}

func currentUser(r *http.Request) *store.User {
	u, _ := r.Context().Value(ctxKey{}).(*store.User)
	return u
}

func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "not logged in")
			return
		}
		u, err := s.Store.SessionUser(r.Context(), auth.HashToken(c.Value), s.now())
		if errors.Is(err, store.ErrNotFound) || (err == nil && u.Disabled) {
			writeError(w, http.StatusUnauthorized, "not logged in")
			return
		}
		if err != nil {
			internalError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, &u)))
	})
}

func requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !currentUser(r).IsAdmin {
			writeError(w, http.StatusForbidden, "admin only")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u store.User) error {
	token, err := auth.NewToken()
	if err != nil {
		return err
	}
	now := s.now()
	if err := s.Store.CreateSession(r.Context(), auth.HashToken(token), u.ID, now, now.Add(sessionTTL)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		MaxAge: int(sessionTTL.Seconds()), HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteStrictMode,
	}) // #nosec G124 -- Secure is off only with KVUI_INSECURE_COOKIES, for local plain-HTTP development
	return nil
}

// hashWait reports whether a password-hash error ends the request (the
// response, if any, has been written). An over-long password is not an
// error here: it simply cannot match, and the request continues as an
// ordinary failed attempt so the response stays indistinguishable.
func (s *Server) hashWait(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil, errors.Is(err, auth.ErrPasswordTooLong):
		return false
	case errors.Is(err, auth.ErrHashBusy):
		writeError(w, http.StatusServiceUnavailable, "server busy, try again shortly")
		return true
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return true // the client has gone away; nothing useful left to send
	default:
		internalError(w, r, err)
		return true
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Password, Code string }
	if !decode(w, r, &req) {
		return
	}
	// Failures count per source and per (account, source). The account
	// counter is scoped to the source so that failures from one address
	// cannot lock the account out for everyone else (including the sole
	// admin); distributed guessing is already impractical against Argon2id
	// plus a required second factor.
	ipKey := s.rateKey(r)
	userKey := "user:" + strings.ToLower(req.Username) + "|" + ipKey
	// Acquire reserves the attempt before the (slow) password check, so a
	// burst of concurrent attempts cannot all pass while none has failed
	// yet; the deferred Release records the outcome.
	acquired := false
	failed := false
	defer func() {
		if acquired {
			s.loginLimiter.Release(userKey, failed)
			s.loginLimiter.Release(ipKey, failed)
		}
	}()
	if !s.loginLimiter.Acquire(userKey) {
		writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	if !s.loginLimiter.Acquire(ipKey) {
		s.loginLimiter.Release(userKey, false)
		writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	acquired = true

	u, err := s.Store.UserByUsername(r.Context(), req.Username)
	ok := false
	switch {
	case errors.Is(err, store.ErrNotFound):
		if s.hashWait(w, r, auth.BurnPasswordCheck(r.Context(), req.Password)) {
			return
		}
	case err != nil:
		internalError(w, r, err)
		return
	default:
		ok, err = auth.VerifyPassword(r.Context(), u.PasswordHash, req.Password)
		if s.hashWait(w, r, err) {
			return
		}
		ok = ok && !u.Disabled
		if ok {
			if ok, err = s.checkSecondFactor(r.Context(), u, req.Code); err != nil {
				internalError(w, r, err)
				return
			}
		}
	}
	if !ok {
		failed = true
		s.audit(r, nil, "login.failed", "", req.Username)
		writeError(w, http.StatusUnauthorized, "invalid username, password or code")
		return
	}

	s.loginLimiter.Reset(userKey)
	if err := s.startSession(w, r, u); err != nil {
		internalError(w, r, err)
		return
	}
	s.audit(r, &u, "login", "", "")
	writeJSON(w, http.StatusOK, u)
}

// checkSecondFactor accepts either a current TOTP code (each time step only
// once) or an unused recovery code.
func (s *Server) checkSecondFactor(ctx context.Context, u store.User, code string) (bool, error) {
	code = strings.TrimSpace(code)
	if len(code) == 6 {
		secret, err := s.Sealer.Open(u.TOTPSecret)
		if err != nil {
			return false, err
		}
		step, ok := auth.VerifyTOTP(secret, code, s.now())
		if !ok {
			return false, nil
		}
		return s.Store.AdvanceTOTPStep(ctx, u.ID, step)
	}
	if code == "" {
		return false, nil
	}
	return s.Store.UseRecoveryCode(ctx, u.ID, auth.HashToken(auth.NormalizeRecoveryCode(code)), s.now())
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := s.Store.DeleteSession(r.Context(), auth.HashToken(c.Value)); err != nil {
			internalError(w, r, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteStrictMode}) // #nosec G124 -- see startSession
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	ms, err := s.Store.Memberships(r.Context(), u.ID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": u, "memberships": nonNil(ms)})
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
