package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSPA(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "static")
	os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("INDEX"), 0o644)
	os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("APP"), 0o644)
	os.WriteFile(filepath.Join(root, "secret"), []byte("SECRET"), 0o644)
	h := spa(dir)

	cases := map[string]string{
		"/":                    "INDEX",
		"/vms/team-a":          "INDEX", // client route
		"/assets/app.js":       "APP",
		"/assets":              "INDEX", // directory
		"/../secret":           "",      // rejected; must not leak
		"/assets/../../secret": "",
	}
	for p, want := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.URL.Path = p // bypass client-side cleaning
		w := httptest.NewRecorder()
		h(w, r)
		if body := w.Body.String(); (want != "" && !strings.Contains(body, want)) || strings.Contains(body, "SECRET") {
			t.Errorf("%s: status %d body %q, want %q", p, w.Code, body, want)
		}
	}

	// HEAD requests succeed with an empty body for SPA routes and static assets.
	for _, p := range []string{"/", "/some-route", "/assets/app.js"} {
		r := httptest.NewRequest(http.MethodHead, p, nil)
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("HEAD %s: status %d, want 200", p, w.Code)
		}
		if w.Body.Len() != 0 {
			t.Errorf("HEAD %s: non-empty body %q", p, w.Body.String())
		}
	}

	// Non-GET/HEAD methods are rejected with 404 JSON.
	r := httptest.NewRequest(http.MethodPost, "/some-route", nil)
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusNotFound || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Errorf("POST /some-route: status %d ct %q", w.Code, w.Header().Get("Content-Type"))
	}
}

func TestHandlerAPINotFoundAndSPA(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("INDEX"), 0o644)

	s := New(Options{StaticDir: dir})
	h := s.Handler()

	// Unknown /api paths return JSON 404 rather than SPA index.html.
	for _, p := range []string{"/api", "/api/", "/api/nonexistent", "//api/nonexistent"} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.URL.Path = p
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404 (body %q)", p, w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("GET %s: Content-Type %q, want application/json", p, ct)
		}
		var errBody struct{ Error string }
		if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil || errBody.Error != "not found" {
			t.Errorf("GET %s: body %q, want JSON not found", p, w.Body.String())
		}
	}

	// Non-/api routes (including paths merely starting with /api-) serve the SPA.
	for _, p := range []string{"/", "/some-route", "/api-docs"} {
		r := httptest.NewRequest(http.MethodGet, p, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "INDEX") {
			t.Errorf("GET %s: status %d body %q", p, w.Code, w.Body.String())
		}
	}

	// HEAD / and HEAD /some-route serve the SPA cleanly.
	for _, p := range []string{"/", "/some-route"} {
		r := httptest.NewRequest(http.MethodHead, p, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK || w.Body.Len() != 0 {
			t.Errorf("HEAD %s: status %d body %q", p, w.Code, w.Body.String())
		}
	}
}
