package api

import (
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
	router := New(Options{StaticDir: dir}).Handler()

	cases := map[string]string{
		"/":                    "INDEX",
		"/vms/team-a":          "INDEX", // client route
		"/assets/app.js":       "APP",
		"/assets":              "INDEX", // directory
		"/../secret":           "",      // rejected; must not leak
		"/assets/../../secret": "",
	}
	for p, want := range cases {
		for label, target := range map[string]http.Handler{"spa": h, "router": router} {
			r := httptest.NewRequest("GET", "/", nil)
			r.URL.Path = p // bypass client-side cleaning
			w := httptest.NewRecorder()
			target.ServeHTTP(w, r)
			if body := w.Body.String(); (want != "" && !strings.Contains(body, want)) || strings.Contains(body, "SECRET") {
				t.Errorf("%s %s: status %d body %q, want %q", label, p, w.Code, body, want)
			}
		}
	}
}
