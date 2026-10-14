package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// A long-lived handler (the VM watch stream is one) must not hold shutdown
// up until the timeout: cancelling the serve context ends its request
// context, the handler returns, and serveHTTP returns promptly.
func TestServeHTTPShutdownEndsLongHandlers(t *testing.T) {
	old := shutdownTimeout
	shutdownTimeout = 5 * time.Second
	defer func() { shutdownTimeout = old }()

	started := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- serveHTTP(ctx, ln, srv) }()

	respErr := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/watch")
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		respErr <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never started")
	}

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serveHTTP: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown held up by the long-lived handler")
	}
	select {
	case <-respErr:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request not ended by shutdown")
	}
}
