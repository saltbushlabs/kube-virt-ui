// Package image manages user-uploaded disk images and ISOs. Each image is
// a CDI DataVolume with an upload source, labelled so the UI can list it;
// VMs clone disk images or attach ISOs as CD-ROMs.
package image

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const (
	Label     = "kubevirt-ui.io/image"
	TypeAnno  = "kubevirt-ui.io/image-type"
	TypeISO   = "iso"
	TypeDisk  = "disk"
	createdBy = "kubevirt-ui.io/created-by"
)

var (
	dvs           = schema.GroupVersionResource{Group: "cdi.kubevirt.io", Version: "v1beta1", Resource: "datavolumes"}
	uploadTokens  = schema.GroupVersionResource{Group: "upload.cdi.kubevirt.io", Version: "v1beta1", Resource: "uploadtokenrequests"}
	minImageSize  = resource.MustParse("100Mi")
	errNotAnImage = errors.New("not an uploaded image")
)

// Image is the UI's view of an uploaded image.
type Image struct {
	Name     string    `json:"name"`
	Type     string    `json:"type"` // iso or disk
	Size     string    `json:"size"`
	Phase    string    `json:"phase"` // CDI phase: UploadScheduled, UploadReady, UploadInProgress, Succeeded, ...
	Progress string    `json:"progress,omitempty"`
	Problem  string    `json:"problem,omitempty"`
	Created  time.Time `json:"created"`
}

// List returns the namespace's uploaded images.
func List(ctx context.Context, dyn dynamic.Interface, namespace string) ([]Image, error) {
	list, err := dyn.Resource(dvs).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: Label + "=true"})
	if apierrors.IsNotFound(err) {
		return []Image{}, nil // CDI not installed
	}
	if err != nil {
		return nil, err
	}
	out := make([]Image, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, summarize(&list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func summarize(dv *unstructured.Unstructured) Image {
	img := Image{Name: dv.GetName(), Type: dv.GetAnnotations()[TypeAnno], Created: dv.GetCreationTimestamp().Time}
	img.Size, _, _ = unstructured.NestedString(dv.Object, "spec", "storage", "resources", "requests", "storage")
	img.Phase, _, _ = unstructured.NestedString(dv.Object, "status", "phase")
	img.Progress, _, _ = unstructured.NestedString(dv.Object, "status", "progress")
	if img.Progress == "N/A" {
		img.Progress = ""
	}
	conds, _, _ := unstructured.NestedSlice(dv.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		reason, _ := m["reason"].(string)
		msg, _ := m["message"].(string)
		if msg != "" && (strings.HasPrefix(reason, "Err") || reason == "Error" || img.Phase == "Failed") {
			img.Problem = msg
			break
		}
	}
	return img
}

// CreateRequest describes a new, empty image awaiting upload.
type CreateRequest struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	Size         string `json:"size"`
	StorageClass string `json:"storageClass,omitempty"`
}

// ValidationError is a problem with the request the user can fix.
type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

func IsValidation(err error) bool {
	var v *ValidationError
	return errors.As(err, &v)
}

func (r *CreateRequest) validate() error {
	r.Name = strings.TrimSpace(r.Name)
	if errs := validation.IsDNS1123Label(r.Name); len(errs) > 0 {
		return &ValidationError{"name: " + strings.Join(errs, "; ")}
	}
	if r.Type != TypeISO && r.Type != TypeDisk {
		return &ValidationError{"type must be iso or disk"}
	}
	size, err := resource.ParseQuantity(r.Size)
	if err != nil || size.Cmp(minImageSize) < 0 {
		return &ValidationError{"size must be at least 100Mi, e.g. 10Gi"}
	}
	r.Size = size.String()
	if r.StorageClass != "" && len(validation.IsDNS1123Subdomain(r.StorageClass)) > 0 {
		return &ValidationError{"invalid storage class"}
	}
	return nil
}

// Create makes an empty upload DataVolume as the caller. CDI then starts an
// upload server for it; the image is ready to receive data once its phase
// is UploadReady.
func Create(ctx context.Context, dyn dynamic.Interface, namespace, user string, r *CreateRequest) error {
	if err := r.validate(); err != nil {
		return err
	}
	storage := map[string]any{"resources": map[string]any{"requests": map[string]any{"storage": r.Size}}}
	if r.StorageClass != "" {
		storage["storageClassName"] = r.StorageClass
	}
	dv := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cdi.kubevirt.io/v1beta1",
		"kind":       "DataVolume",
		"metadata": map[string]any{
			"name":      r.Name,
			"namespace": namespace,
			"labels":    map[string]any{Label: "true"},
			"annotations": map[string]any{
				TypeAnno:  r.Type,
				createdBy: user,
				// Images have no consuming VM yet. Without this, storage
				// classes with WaitForFirstConsumer binding never start
				// the upload server.
				"cdi.kubevirt.io/storage.bind.immediate.requested": "true",
			},
		},
		"spec": map[string]any{
			"source":  map[string]any{"upload": map[string]any{}},
			"storage": storage,
		},
	}}
	_, err := dyn.Resource(dvs).Namespace(namespace).Create(ctx, dv, metav1.CreateOptions{})
	return err
}

// Delete removes an uploaded image. VMs still using it keep the volume
// until they are deleted (PVC protection).
func Delete(ctx context.Context, dyn dynamic.Interface, namespace, name string) error {
	if _, err := get(ctx, dyn, namespace, name); err != nil {
		return err
	}
	return dyn.Resource(dvs).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

func get(ctx context.Context, dyn dynamic.Interface, namespace, name string) (*unstructured.Unstructured, error) {
	dv, err := dyn.Resource(dvs).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if dv.GetLabels()[Label] != "true" {
		return nil, &ValidationError{errNotAnImage.Error()}
	}
	return dv, nil
}

// Type returns an image's type, or a validation error if name is not an
// uploaded image in namespace.
func Type(ctx context.Context, dyn dynamic.Interface, namespace, name string) (string, error) {
	dv, err := get(ctx, dyn, namespace, name)
	if err != nil {
		return "", err
	}
	return dv.GetAnnotations()[TypeAnno], nil
}

// UploadProxy sends image data to CDI's upload proxy service.
type UploadProxy struct {
	URL        string // e.g. https://cdi-uploadproxy.cdi.svc
	ServerName string // TLS name to verify; defaults to the URL's host
	// CA bundle the proxy's certificate chains to. CDI rotates it, so it
	// is read on every upload.
	CAConfigMapNamespace, CAConfigMapName, CAConfigMapKey string
	Kube                                                  kubernetes.Interface // backend identity, reads the CA bundle
}

func (p *UploadProxy) client(ctx context.Context) (*http.Client, error) {
	cm, err := p.Kube.CoreV1().ConfigMaps(p.CAConfigMapNamespace).Get(ctx, p.CAConfigMapName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read CDI upload proxy CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(cm.Data[p.CAConfigMapKey])) {
		return nil, fmt.Errorf("no certificates in %s/%s", p.CAConfigMapNamespace, p.CAConfigMapName)
	}
	serverName := p.ServerName
	if serverName == "" {
		u, err := url.Parse(p.URL)
		if err != nil {
			return nil, err
		}
		serverName = u.Hostname()
	}
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS12},
		Proxy:           http.ProxyFromEnvironment,
		// With Expect: 100-continue the body is only sent once the proxy
		// has reached the upload server; see Upload.
		ExpectContinueTimeout: 30 * time.Second,
	}}, nil
}

// Upload streams body into image name. The upload token is requested with
// the caller's dyn client, so only users allowed to create
// uploadtokenrequests in namespace (owners) can upload.
func (p *UploadProxy) Upload(ctx context.Context, dyn dynamic.Interface, namespace, name string, body io.Reader, length int64) error {
	if _, err := get(ctx, dyn, namespace, name); err != nil {
		return err
	}
	tr := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "upload.cdi.kubevirt.io/v1beta1",
		"kind":       "UploadTokenRequest",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec":       map[string]any{"pvcName": name},
	}}
	tr, err := dyn.Resource(uploadTokens).Namespace(namespace).Create(ctx, tr, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	token, _, _ := unstructured.NestedString(tr.Object, "status", "token")
	if token == "" {
		return errors.New("CDI returned an empty upload token")
	}

	client, err := p.client(ctx)
	if err != nil {
		return err
	}
	// The client and its Transport are built per upload (the CA bundle is
	// re-read each time); drop its idle connection instead of leaving it
	// and its goroutines open against the upload proxy.
	defer client.CloseIdleConnections()
	// CDI marks an image UploadReady when its upload server pod starts,
	// a moment before the server accepts connections, so the proxy can
	// briefly answer 502/503. With Expect: 100-continue it does so before
	// reading any of the body, and the attempt is retried with the same,
	// still unread, stream.
	counted := &countingReader{r: body}
	for attempt := 1; ; attempt++ {
		err := p.send(ctx, client, token, counted, length)
		var rejected *UploadError
		if !errors.As(err, &rejected) || counted.n > 0 || attempt == uploadAttempts ||
			(rejected.Status != http.StatusBadGateway && rejected.Status != http.StatusServiceUnavailable) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

const uploadAttempts = 10

func (p *UploadProxy) send(ctx context.Context, client *http.Client, token string, body io.Reader, length int64) error {
	// upload-async returns once the data is received; CDI then converts
	// and resizes it, which shows up as the image's phase.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.URL, "/")+"/v1beta1/upload-async", io.NopCloser(body))
	if err != nil {
		return err
	}
	req.ContentLength = length
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Expect", "100-continue")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("upload proxy: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &UploadError{Status: resp.StatusCode, Message: strings.TrimSpace(string(msg))}
	}
	return nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// UploadError is a refusal from CDI's upload proxy, e.g. the image is not
// ready to receive data yet.
type UploadError struct {
	Status  int
	Message string
}

func (e *UploadError) Error() string {
	return fmt.Sprintf("upload rejected (%d): %s", e.Status, e.Message)
}
