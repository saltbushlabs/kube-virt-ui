package vm

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
)

func TestDialConsoleRejectsPathNames(t *testing.T) {
	// No server behind Host: a bad name must be refused before any dial.
	cfg := &rest.Config{Host: "https://127.0.0.1:1"}
	for _, name := range []string{"", "..", "../../other-ns/virtualmachineinstances/web", "web/console", "Web"} {
		_, err := DialConsole(context.Background(), cfg, "team-a", name, "vnc")
		if !apierrors.IsNotFound(err) {
			t.Errorf("name %q: got %v, want not found", name, err)
		}
	}
}
