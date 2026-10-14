package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"kvui/internal/store"
)

// fakeKubeVirt answers the few KubeVirt endpoints the VM handlers use and
// records "METHOD path token" for each request.
func (e *env) fakeKubeVirt(w http.ResponseWriter, r *http.Request) {
	e.requests = append(e.requests, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
	if r.URL.Query().Get("watch") == "true" {
		// Hold the watch open without sending events until the client goes away.
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
		return
	}
	if strings.HasPrefix(r.URL.Path, "/apis/subresources.kubevirt.io/v1/namespaces/team-a/virtualmachineinstances/web/") {
		fakeConsole(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.Method + " " + r.URL.Path {
	case "GET /apis/kubevirt.io/v1/namespaces/team-a/virtualmachines":
		w.Write([]byte(`{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachineList","items":[
			{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachine","metadata":{"name":"web","namespace":"team-a"},
			 "spec":{"runStrategy":"Always"},"status":{"printableStatus":"Running","ready":true}}]}`))
	case "GET /apis/kubevirt.io/v1/namespaces/team-a/virtualmachineinstances":
		w.Write([]byte(`{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachineInstanceList","items":[
			{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachineInstance","metadata":{"name":"web","namespace":"team-a"},
			 "status":{"nodeName":"node-1","interfaces":[{"ipAddress":"10.0.0.5"}]}}]}`))
	case "POST /apis/kubevirt.io/v1/namespaces/team-a/virtualmachines":
		body, _ := io.ReadAll(r.Body)
		e.created = body
		w.WriteHeader(http.StatusCreated)
		w.Write(body)
	case "GET /apis/cdi.kubevirt.io/v1beta1/namespaces/team-a/datavolumes/iso":
		w.Write([]byte(`{"apiVersion":"cdi.kubevirt.io/v1beta1","kind":"DataVolume","metadata":{"name":"iso","namespace":"team-a",
			"labels":{"kubevirt-ui.io/image":"true"},"annotations":{"kubevirt-ui.io/image-type":"iso"}}}`))
	case "GET /apis/cdi.kubevirt.io/v1beta1/namespaces/team-a/datavolumes/web-root":
		w.Write([]byte(`{"apiVersion":"cdi.kubevirt.io/v1beta1","kind":"DataVolume","metadata":{"name":"web-root","namespace":"team-a"}}`))
	case "POST /apis/upload.cdi.kubevirt.io/v1beta1/namespaces/team-a/uploadtokenrequests":
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"apiVersion":"upload.cdi.kubevirt.io/v1beta1","kind":"UploadTokenRequest","metadata":{"name":"iso"},
			"status":{"token":"upload-token"}}`))
	case "PUT /apis/subresources.kubevirt.io/v1/namespaces/team-a/virtualmachines/web/restart":
		w.WriteHeader(http.StatusAccepted)
	case "PUT /apis/subresources.kubevirt.io/v1/namespaces/team-a/virtualmachines/web/stop":
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,
			"message":"virtualmachines/stop is forbidden"}`))
	case "GET /apis/subresources.kubevirt.io/v1/namespaces/team-a/virtualmachineinstances/off/vnc":
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404,
			"message":"virtualmachineinstance off not found"}`))
	default:
		http.NotFound(w, r)
	}
}

func TestVMListAndActionsUseUserToken(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, link, _ := CreateInvite(ctx, e.store, "https://ui.test", store.Invite{
		Namespace: "team-a", Role: "operator", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	alice := e.client()
	e.redeem(alice, link, "alice")

	var vms []struct {
		Name, Status, Node string
		IPs                []string
	}
	if st := e.do(alice, "GET", "/api/namespaces/team-a/vms", nil, &vms); st != 200 {
		t.Fatalf("list: %d", st)
	}
	if len(vms) != 1 || vms[0].Status != "Running" || vms[0].Node != "node-1" || vms[0].IPs[0] != "10.0.0.5" {
		t.Fatalf("vms: %+v", vms)
	}

	if st := e.do(alice, "POST", "/api/namespaces/team-a/vms/web/restart", nil, nil); st != 204 {
		t.Fatalf("restart: %d", st)
	}
	var errBody struct{ Error string }
	if st := e.do(alice, "POST", "/api/namespaces/team-a/vms/web/stop", nil, &errBody); st != 403 || errBody.Error != "virtualmachines/stop is forbidden" {
		t.Fatalf("forbidden passthrough: %d %+v", st, errBody)
	}
	if st := e.do(alice, "POST", "/api/namespaces/team-a/vms/web/destroy", nil, nil); st != 404 {
		t.Fatalf("unknown action: %d", st)
	}
	if st := e.do(alice, "GET", "/api/namespaces/other/vms", nil, nil); st != 403 {
		t.Fatalf("non-member namespace: %d", st)
	}

	// Every call reached the apiserver with alice's ServiceAccount token,
	// never the backend's own credentials.
	for _, r := range e.requests {
		if want := "Bearer tok-team-a-kvui-u1"; r[len(r)-len(want):] != want {
			t.Errorf("request not made as alice: %s", r)
		}
	}
	if len(e.requests) != 5 { // VMs, VMIs, DataVolumes (404: CDI absent), restart, stop
		t.Errorf("requests: %v", e.requests)
	}
}

func TestDelabelledNamespaceRefused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, link, _ := CreateInvite(ctx, e.store, "https://ui.test", store.Invite{
		Namespace: "team-a", Role: "operator", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	alice := e.client()
	e.redeem(alice, link, "alice")
	if st := e.do(alice, "GET", "/api/namespaces/team-a/vms", nil, nil); st != 200 {
		t.Fatalf("list: %d", st)
	}

	// The membership row stays, but the namespace is no longer a tenant, so
	// the cached token must not be used.
	e.setTenant("team-a", false)
	before := len(e.requests)
	var errBody struct{ Error string }
	if st := e.do(alice, "GET", "/api/namespaces/team-a/vms", nil, &errBody); st != 403 || errBody.Error == "" {
		t.Fatalf("de-labelled namespace: %d %+v", st, errBody)
	}
	if len(e.requests) != before {
		t.Fatalf("apiserver called for de-labelled namespace: %v", e.requests[before:])
	}
}

// An open watch stream must end promptly when access is revoked, not
// serve VM data until the five-minute cut: the ServiceAccount token it
// lists with is unaffected by logout, disable or membership removal.
func TestWatchVMsEndsOnRevocation(t *testing.T) {
	e := newEnv(t)
	e.api.consoleRecheck = 50 * time.Millisecond
	ctx := context.Background()
	_, link, _ := CreateInvite(ctx, e.store, "https://ui.test", store.Invite{
		Namespace: "team-a", Role: "operator", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	alice := e.client()
	e.redeem(alice, link, "alice")

	req, _ := http.NewRequest("GET", e.srv.URL+"/api/namespaces/team-a/watch/vms", nil)
	req.Header.Set("X-Requested-With", "kvui")
	resp, err := alice.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("watch: %d", resp.StatusCode)
	}
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	// The initial list arrives.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream ended before the initial list")
			}
			if strings.Contains(line, `"web"`) {
				goto listed
			}
		case <-deadline:
			t.Fatal("no initial VM list on the stream")
		}
	}
listed:

	// Logging out ends the stream even though no VM changes arrive.
	if st := e.do(alice, "POST", "/api/logout", nil, nil); st != 204 {
		t.Fatalf("logout: %d", st)
	}
	deadline = time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-lines:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("watch stream stayed open after logout")
		}
	}
}

// CloseStreams (wired to http.Server.RegisterOnShutdown in main) ends an
// open watch stream so graceful shutdown is not held up by it, without
// cancelling ordinary request contexts.
func TestWatchVMsEndsOnCloseStreams(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, link, _ := CreateInvite(ctx, e.store, "https://ui.test", store.Invite{
		Namespace: "team-a", Role: "operator", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	alice := e.client()
	e.redeem(alice, link, "alice")

	req, _ := http.NewRequest("GET", e.srv.URL+"/api/namespaces/team-a/watch/vms", nil)
	req.Header.Set("X-Requested-With", "kvui")
	resp, err := alice.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("watch: %d", resp.StatusCode)
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		close(done)
	}()
	e.api.CloseStreams()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watch stream stayed open after CloseStreams")
	}
}

// A membership deleted while a request is re-provisioning (fault-injected
// here by deleting the row inside Ensure's RoleBinding delete) must refuse
// the request and remove the just-recreated ServiceAccount/RoleBinding.
func TestClientsForUndoesProvisioningAfterRemoval(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, link, _ := CreateInvite(ctx, e.store, "https://ui.test", store.Invite{
		Namespace: "team-a", Role: "operator", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	alice := e.client()
	e.redeem(alice, link, "alice") // user 1, provisioned as operator

	// Queue a role change so the next request re-runs Ensure, and delete
	// the membership row in the middle of it (the RoleBinding delete).
	if err := e.store.UpsertMembership(ctx, store.Membership{UserID: 1, Namespace: "team-a", Role: "owner"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	var injected atomic.Bool
	e.k8s.PrependReactor("delete", "rolebindings", func(k8stesting.Action) (bool, runtime.Object, error) {
		if injected.CompareAndSwap(false, true) {
			_ = e.store.DeleteMembership(ctx, 1, "team-a")
		}
		return false, nil, nil // let the delete proceed
	})

	if st := e.do(alice, "GET", "/api/namespaces/team-a/vms", nil, nil); st != 403 {
		t.Fatalf("request for removed membership: %d", st)
	}
	if !injected.Load() {
		t.Fatal("fault was never injected; Ensure did not run")
	}
	if _, err := e.k8s.CoreV1().ServiceAccounts("team-a").Get(ctx, "kvui-u1", metav1.GetOptions{}); err == nil {
		t.Fatal("serviceaccount resurrected for removed membership")
	}
	if _, err := e.k8s.RbacV1().RoleBindings("team-a").Get(ctx, "kvui-u1", metav1.GetOptions{}); err == nil {
		t.Fatal("rolebinding resurrected for removed membership")
	}
}

// The admin removal path removes the Kubernetes objects again after
// deleting the row, so a ServiceAccount/RoleBinding recreated by a racing
// request in between (fault-injected here at the first ServiceAccount
// delete) does not survive.
func TestAdminDeleteMembershipRemovesRecreated(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, adminLink, _ := CreateInvite(ctx, e.store, "https://ui.test", store.Invite{
		MakeAdmin: true, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	admin := e.client()
	e.redeem(admin, adminLink, "root")
	_, link, _ := CreateInvite(ctx, e.store, "https://ui.test", store.Invite{
		Namespace: "team-a", Role: "operator", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	e.redeem(e.client(), link, "alice") // user 2

	saGVR := corev1.SchemeGroupVersion.WithResource("serviceaccounts")
	var recreated atomic.Bool
	e.k8s.PrependReactor("delete", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() != "team-a" || !recreated.CompareAndSwap(false, true) {
			return false, nil, nil
		}
		// Perform the delete, then simulate a racing Ensure recreating
		// both objects before the membership row is deleted.
		if err := e.k8s.Tracker().Delete(saGVR, "team-a", "kvui-u2"); err != nil {
			return true, nil, err
		}
		meta := metav1.ObjectMeta{Name: "kvui-u2", Namespace: "team-a"}
		if err := e.k8s.Tracker().Add(&corev1.ServiceAccount{ObjectMeta: meta}); err != nil {
			return true, nil, err
		}
		err := e.k8s.Tracker().Add(&rbacv1.RoleBinding{ObjectMeta: meta})
		return true, nil, err
	})

	if st := e.do(admin, "DELETE", "/api/admin/users/2/memberships/team-a", nil, nil); st != 204 {
		t.Fatalf("remove membership: %d", st)
	}
	if !recreated.Load() {
		t.Fatal("fault was never injected; ServiceAccount was not deleted")
	}
	if _, err := e.k8s.CoreV1().ServiceAccounts("team-a").Get(ctx, "kvui-u2", metav1.GetOptions{}); err == nil {
		t.Fatal("recreated serviceaccount survived membership removal")
	}
	if _, err := e.k8s.RbacV1().RoleBindings("team-a").Get(ctx, "kvui-u2", metav1.GetOptions{}); err == nil {
		t.Fatal("recreated rolebinding survived membership removal")
	}
}

func TestCreateVM(t *testing.T) {
	e := newEnv(t)
	_, link, _ := CreateInvite(context.Background(), e.store, "https://ui.test", store.Invite{
		Namespace: "team-a", Role: "owner", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	alice := e.client()
	e.redeem(alice, link, "alice")

	var errBody struct{ Error string }
	bad := map[string]any{"name": "web", "cpus": 2, "memory": "4Gi", "diskSource": "http", "diskUrl": "http://169.254.169.254/x", "diskSize": "10Gi"}
	if st := e.do(alice, "POST", "/api/namespaces/team-a/vms", bad, &errBody); st != 400 || e.created != nil {
		t.Fatalf("invalid request reached apiserver: %d %s", st, errBody.Error)
	}

	req := map[string]any{"name": "web", "instancetype": map[string]string{"name": "u1.small", "kind": "VirtualMachineClusterInstancetype"},
		"diskSource": "registry", "diskUrl": "quay.io/containerdisks/fedora:latest", "diskSize": "20Gi", "start": true}
	if st := e.do(alice, "POST", "/api/namespaces/team-a/vms", req, nil); st != 201 {
		t.Fatalf("create: %d", st)
	}
	var vm map[string]any
	if err := json.Unmarshal(e.created, &vm); err != nil {
		t.Fatal(err)
	}
	spec := vm["spec"].(map[string]any)
	if spec["instancetype"].(map[string]any)["name"] != "u1.small" || spec["runStrategy"] != "Always" {
		t.Fatalf("spec: %v", spec)
	}
	if last := e.requests[len(e.requests)-1]; !strings.HasSuffix(last, "Bearer tok-team-a-kvui-u1") {
		t.Fatalf("not created as alice: %s", last)
	}
}
