package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func serveForTest(t *testing.T, srv *http.Server) (addr string, cancel context.CancelFunc, serveErr chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveErr = make(chan error, 1)
	go func() { serveErr <- serveHTTP(ctx, ln, srv) }()
	return ln.Addr().String(), cancel, serveErr
}

// A long-lived stream handler (the VM watch stream is one) must not hold
// shutdown up until the timeout. Streams watch a context that the
// RegisterOnShutdown hook cancels when shutdown begins.
func TestServeHTTPShutdownEndsStreams(t *testing.T) {
	old := shutdownTimeout
	shutdownTimeout = 5 * time.Second
	defer func() { shutdownTimeout = old }()

	streamCtx, closeStreams := context.WithCancel(context.Background())
	started := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-streamCtx.Done()
	})}
	srv.RegisterOnShutdown(closeStreams)
	addr, cancel, serveErr := serveForTest(t, srv)

	respErr := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/watch")
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
		t.Fatal("shutdown held up by the stream handler")
	}
	select {
	case <-respErr:
	case <-time.After(2 * time.Second):
		t.Fatal("stream request not ended by shutdown")
	}
}

// An ordinary in-flight request is not cancelled by shutdown: it keeps
// its request context and finishes while Shutdown drains.
func TestServeHTTPShutdownDrainsRequests(t *testing.T) {
	started := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(200 * time.Millisecond) // a write that must be allowed to finish
		if r.Context().Err() != nil {
			t.Error("request context cancelled by shutdown")
		}
		_, _ = w.Write([]byte("done"))
	})}
	addr, cancel, serveErr := serveForTest(t, srv)

	type result struct {
		body string
		err  error
	}
	res := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/create")
		if err != nil {
			res <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		res <- result{string(b), err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never started")
	}

	cancel() // shutdown begins while the request is in flight
	select {
	case r := <-res:
		if r.err != nil || r.body != "done" {
			t.Fatalf("in-flight request cut off by shutdown: %q %v", r.body, r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight request never completed")
	}
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serveHTTP: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serveHTTP did not return after draining")
	}
}
