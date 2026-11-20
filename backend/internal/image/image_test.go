package image

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/kubernetes/fake"
)

func TestValidate(t *testing.T) {
	for name, r := range map[string]CreateRequest{
		"bad name": {Name: "My ISO", Type: TypeISO, Size: "5Gi"},
		"bad type": {Name: "a", Type: "exe", Size: "5Gi"},
		"tiny":     {Name: "a", Type: TypeISO, Size: "1Mi"},
		"bad size": {Name: "a", Type: TypeDisk, Size: "big"},
	} {
		if err := r.validate(); !IsValidation(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	r := CreateRequest{Name: "fedora-iso", Type: TypeISO, Size: "2048Mi"}
	if err := r.validate(); err != nil || r.Size != "2Gi" {
		t.Fatalf("%v %+v", err, r)
	}
}

func TestSummarize(t *testing.T) {
	dv := &unstructured.Unstructured{}
	json.Unmarshal([]byte(`{"metadata":{"name":"iso","annotations":{"kubevirt-ui.io/image-type":"iso"}},
		"spec":{"storage":{"resources":{"requests":{"storage":"5Gi"}}}},
		"status":{"phase":"UploadReady","progress":"N/A","conditions":[{"type":"Running","status":"True","reason":"Pod is running"}]}}`), &dv.Object)
	img := summarize(dv)
	if img.Type != "iso" || img.Size != "5Gi" || img.Phase != "UploadReady" || img.Progress != "" || img.Problem != "" {
		t.Fatalf("%+v", img)
	}
}

func TestCountingReaderConcurrent(t *testing.T) {
	const total = 64 * 1024
	cr := &countingReader{r: bytes.NewReader(make([]byte, total))}
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		for {
			_, err := cr.Read(buf)
			if err == io.EOF {
				done <- nil
				return
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()
	for {
		n := cr.n.Load()
		if n < 0 || n > total {
			t.Fatalf("out-of-range count: %d", n)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			if got := cr.n.Load(); got != total {
				t.Fatalf("count = %d, want %d", got, total)
			}
			return
		default:
		}
	}
}

func TestUploadProxyClientTransport(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	kube := fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cdi-uploadproxy-signer-bundle", Namespace: "cdi"},
		Data:       map[string]string{"ca-bundle.crt": string(caPEM)},
	})
	p := &UploadProxy{
		URL:                  "https://cdi-uploadproxy.cdi.svc",
		CAConfigMapNamespace: "cdi",
		CAConfigMapName:      "cdi-uploadproxy-signer-bundle",
		CAConfigMapKey:       "ca-bundle.crt",
		Kube:                 kube,
	}
	client, err := p.client(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	def := http.DefaultTransport.(*http.Transport)
	if tr == def {
		t.Fatal("transport was not cloned from http.DefaultTransport")
	}
	if tr.DialContext == nil {
		t.Error("DialContext is nil; default dial timeout lost")
	}
	if tr.TLSHandshakeTimeout != def.TLSHandshakeTimeout || tr.TLSHandshakeTimeout <= 0 {
		t.Errorf("TLSHandshakeTimeout = %v, want %v", tr.TLSHandshakeTimeout, def.TLSHandshakeTimeout)
	}
	if tr.IdleConnTimeout != def.IdleConnTimeout || tr.IdleConnTimeout <= 0 {
		t.Errorf("IdleConnTimeout = %v, want %v", tr.IdleConnTimeout, def.IdleConnTimeout)
	}
	if tr.ExpectContinueTimeout != 30*time.Second {
		t.Errorf("ExpectContinueTimeout = %v, want 30s", tr.ExpectContinueTimeout)
	}
	if def.ExpectContinueTimeout == 30*time.Second {
		t.Error("http.DefaultTransport was mutated")
	}
	if tr.Proxy == nil {
		t.Error("Proxy is nil")
	}
	if tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = true, want false to keep HTTP/1.1 for cdi-uploadproxy")
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 || tr.TLSClientConfig.ServerName != "cdi-uploadproxy.cdi.svc" {
		t.Errorf("unexpected TLSClientConfig: %+v", tr.TLSClientConfig)
	}
}
