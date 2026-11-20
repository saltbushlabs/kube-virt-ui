package vm

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func obj(t *testing.T, s string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	if err := json.Unmarshal([]byte(s), &u.Object); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestSummarizeRunning(t *testing.T) {
	vm := obj(t, `{"metadata":{"name":"web","namespace":"team-a"},
		"spec":{"runStrategy":"Always","template":{"spec":{"domain":{
			"cpu":{"cores":2,"sockets":2},"memory":{"guest":"4Gi"}}}}},
		"status":{"printableStatus":"Running","ready":true}}`)
	vmi := obj(t, `{"status":{"nodeName":"node-1","guestOSInfo":{"prettyName":"Fedora Linux 42"},
		"interfaces":[{"ipAddress":"10.0.0.5"},{"name":"no-ip"}]}}`)
	s := summarize(vm, vmi, nil)
	if s.Status != "Running" || !s.Ready || s.CPUs != 4 || s.Memory != "4Gi" ||
		s.Node != "node-1" || s.OS != "Fedora Linux 42" || len(s.IPs) != 1 || s.IPs[0] != "10.0.0.5" {
		t.Fatalf("%+v", s)
	}
}

func TestSummarizeStoppedLegacyAndInstancetype(t *testing.T) {
	legacy := summarize(obj(t, `{"metadata":{"name":"old"},"spec":{"running":false,"template":{"spec":{"domain":{
		"resources":{"requests":{"memory":"1024Mi"}}}}}}}`), nil, nil)
	if legacy.RunStrategy != "Halted" || legacy.Status != "Unknown" || legacy.CPUs != 1 || legacy.Memory != "1Gi" {
		t.Fatalf("legacy: %+v", legacy)
	}
	it := summarize(obj(t, `{"metadata":{"name":"it"},"spec":{"runStrategy":"Halted","instancetype":{"name":"u1.medium"}},
		"status":{"printableStatus":"Stopped"}}`), nil, nil)
	if it.Instancetype != "u1.medium" || it.CPUs != 0 || it.Memory != "" || it.Status != "Stopped" {
		t.Fatalf("instancetype: %+v", it)
	}
}

func TestSummarizeIgnoresLingeringInstanceWhenStopped(t *testing.T) {
	s := summarize(obj(t, `{"metadata":{"name":"x"},"status":{"printableStatus":"Stopped"}}`),
		obj(t, `{"status":{"nodeName":"node-1","interfaces":[{"ipAddress":"10.0.0.5"}]}}`), nil)
	if s.Node != "" || len(s.IPs) != 0 {
		t.Fatalf("%+v", s)
	}
}

// A non-object entry in the volumes list must be skipped, not panic the
// whole VM list.
func TestSummarizeMalformedVolume(t *testing.T) {
	vm := obj(t, `{"metadata":{"name":"web"},"status":{"printableStatus":"Running"},
		"spec":{"template":{"spec":{"volumes":[null,"junk",{"name":"root","dataVolume":{"name":"web-root"}}]}}}}`)
	dvs := map[string]*unstructured.Unstructured{
		"web-root": obj(t, `{"metadata":{"name":"web-root"},"status":{"phase":"Succeeded"}}`),
	}
	s := summarize(vm, nil, dvs)
	if len(s.Disks) != 1 || s.Disks[0].Name != "web-root" {
		t.Fatalf("%+v", s.Disks)
	}
}

func TestSummarizeDisks(t *testing.T) {
	vm := obj(t, `{"metadata":{"name":"web"},"status":{"printableStatus":"Provisioning"},
		"spec":{"template":{"spec":{"volumes":[{"name":"root","dataVolume":{"name":"web-root"}},{"name":"ci","cloudInitNoCloud":{}}]}}}}`)
	dvs := map[string]*unstructured.Unstructured{
		"web-root": obj(t, `{"metadata":{"name":"web-root"},"status":{"phase":"Pending","progress":"N/A","conditions":[
			{"type":"Bound","status":"False","reason":"ErrClaimNotValid","message":"no accessMode specified in StorageProfile local-path"}]}}`),
		"other": obj(t, `{"metadata":{"name":"other"},"status":{"phase":"Succeeded"}}`),
	}
	s := summarize(vm, nil, dvs)
	if len(s.Disks) != 1 || s.Disks[0].Problem != "no accessMode specified in StorageProfile local-path" || s.Disks[0].Progress != "" {
		t.Fatalf("%+v", s.Disks)
	}
	dvs["web-root"] = obj(t, `{"metadata":{"name":"web-root"},"status":{"phase":"ImportInProgress","progress":"45.20%","conditions":[
		{"type":"Running","status":"True","reason":"Pod is running"}]}}`)
	if d := summarize(vm, nil, dvs).Disks[0]; d.Progress != "45.20%" || d.Problem != "" {
		t.Fatalf("%+v", d)
	}
}

func newFakeDynamic(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{
		VMs:  "VirtualMachineList",
		VMIs: "VirtualMachineInstanceList",
		DVs:  "DataVolumeList",
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, objects...)
}

func TestListWithRVsAndWatchPassesResourceVersions(t *testing.T) {
	vmObj := obj(t, `{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachine","metadata":{"name":"web","namespace":"team-a"},
		"spec":{"runStrategy":"Always"},"status":{"printableStatus":"Running","ready":true}}`)
	vmiObj := obj(t, `{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachineInstance","metadata":{"name":"web","namespace":"team-a"},
		"status":{"nodeName":"node-1","interfaces":[{"ipAddress":"10.0.0.5"}]}}`)
	dvObj := obj(t, `{"apiVersion":"cdi.kubevirt.io/v1beta1","kind":"DataVolume","metadata":{"name":"web-root","namespace":"team-a"},
		"status":{"phase":"Succeeded"}}`)

	t.Run("with CDI", func(t *testing.T) {
		dyn := newFakeDynamic(vmObj, vmiObj, dvObj)
		gvkByResource := map[string]schema.GroupVersionKind{
			"virtualmachines":         VMs.GroupVersion().WithKind("VirtualMachine"),
			"virtualmachineinstances": VMIs.GroupVersion().WithKind("VirtualMachineInstance"),
			"datavolumes":             DVs.GroupVersion().WithKind("DataVolume"),
		}
		rvByResource := map[string]string{
			"virtualmachines":         "101",
			"virtualmachineinstances": "202",
			"datavolumes":             "303",
		}
		dyn.PrependReactor("list", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
			res := a.GetResource().Resource
			obj, err := dyn.Tracker().List(a.GetResource(), gvkByResource[res], a.GetNamespace())
			if err != nil {
				return true, nil, err
			}
			list := obj.(*unstructured.UnstructuredList)
			list.SetResourceVersion(rvByResource[res])
			return true, list, nil
		})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		vms, rvs, err := ListWithRVs(ctx, dyn, "team-a")
		if err != nil {
			t.Fatalf("ListWithRVs: %v", err)
		}
		if len(vms) != 1 || vms[0].Name != "web" {
			t.Fatalf("vms: %+v", vms)
		}
		wantRVs := WatchVersions{VMs: "101", VMIs: "202", DVs: "303"}
		if rvs != wantRVs {
			t.Fatalf("rvs = %+v, want %+v", rvs, wantRVs)
		}

		var mu sync.Mutex
		gotWatchRVs := map[string]string{}
		vmWatcher := watch.NewFake()
		dyn.PrependWatchReactor("*", func(a k8stesting.Action) (bool, watch.Interface, error) {
			wa := a.(k8stesting.WatchAction)
			mu.Lock()
			gotWatchRVs[a.GetResource().Resource] = wa.GetWatchRestrictions().ResourceVersion
			mu.Unlock()
			if a.GetResource().Resource == "virtualmachines" {
				return true, vmWatcher, nil
			}
			return true, watch.NewFake(), nil
		})

		changes, err := Watch(ctx, dyn, "team-a", rvs)
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}
		mu.Lock()
		for res, want := range rvByResource {
			if got := gotWatchRVs[res]; got != want {
				t.Errorf("watch %s resourceVersion = %q, want %q", res, got, want)
			}
		}
		mu.Unlock()

		// No spurious signal before an event is emitted.
		select {
		case <-changes:
			t.Fatal("unexpected change signal before any watch event")
		case <-time.After(30 * time.Millisecond):
		}

		vmWatcher.Modify(vmObj)
		select {
		case _, ok := <-changes:
			if !ok {
				t.Fatal("changes closed unexpectedly")
			}
		case <-time.After(time.Second):
			t.Fatal("expected change signal after watch event")
		}

		cancel()
		select {
		case _, ok := <-changes:
			if ok {
				t.Fatal("expected changes channel to close after cancel")
			}
		case <-time.After(time.Second):
			t.Fatal("changes did not close after cancel")
		}
	})

	t.Run("without CDI", func(t *testing.T) {
		dyn := newFakeDynamic(vmObj, vmiObj)
		gvkByResource := map[string]schema.GroupVersionKind{
			"virtualmachines":         VMs.GroupVersion().WithKind("VirtualMachine"),
			"virtualmachineinstances": VMIs.GroupVersion().WithKind("VirtualMachineInstance"),
		}
		dyn.PrependReactor("list", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
			if a.GetResource().Resource == "datavolumes" {
				return true, nil, apierrors.NewNotFound(DVs.GroupResource(), "")
			}
			obj, err := dyn.Tracker().List(a.GetResource(), gvkByResource[a.GetResource().Resource], a.GetNamespace())
			if err != nil {
				return true, nil, err
			}
			list := obj.(*unstructured.UnstructuredList)
			switch a.GetResource().Resource {
			case "virtualmachines":
				list.SetResourceVersion("11")
			case "virtualmachineinstances":
				list.SetResourceVersion("22")
			}
			return true, list, nil
		})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		vms, rvs, err := ListWithRVs(ctx, dyn, "team-a")
		if err != nil {
			t.Fatalf("ListWithRVs without CDI: %v", err)
		}
		if len(vms) != 1 || rvs != (WatchVersions{VMs: "11", VMIs: "22", DVs: ""}) {
			t.Fatalf("vms=%+v rvs=%+v", vms, rvs)
		}

		var mu sync.Mutex
		gotWatchRVs := map[string]string{}
		dyn.PrependWatchReactor("*", func(a k8stesting.Action) (bool, watch.Interface, error) {
			wa := a.(k8stesting.WatchAction)
			mu.Lock()
			gotWatchRVs[a.GetResource().Resource] = wa.GetWatchRestrictions().ResourceVersion
			mu.Unlock()
			if a.GetResource().Resource == "datavolumes" {
				return true, nil, apierrors.NewNotFound(DVs.GroupResource(), "")
			}
			return true, watch.NewFake(), nil
		})

		changes, err := Watch(ctx, dyn, "team-a", rvs)
		if err != nil {
			t.Fatalf("Watch without CDI: %v", err)
		}
		mu.Lock()
		if gotWatchRVs["virtualmachines"] != "11" || gotWatchRVs["virtualmachineinstances"] != "22" || gotWatchRVs["datavolumes"] != "" {
			t.Fatalf("watch RVs = %+v", gotWatchRVs)
		}
		mu.Unlock()

		cancel()
		select {
		case _, ok := <-changes:
			if ok {
				t.Fatal("expected changes channel to close")
			}
		case <-time.After(time.Second):
			t.Fatal("changes did not close after cancel")
		}
	})
}

func TestGetTargeted(t *testing.T) {
	webVM := obj(t, `{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachine","metadata":{"name":"web","namespace":"team-a"},
		"spec":{"runStrategy":"Always","template":{"spec":{
			"domain":{"cpu":{"cores":2},"memory":{"guest":"4Gi"}},
			"volumes":[
				{"name":"root","dataVolume":{"name":"web-root"}},
				{"name":"data","dataVolume":{"name":"web-data"}},
				{"name":"dup","dataVolume":{"name":"web-root"}},
				{"name":"missing","dataVolume":{"name":"web-missing"}},
				{"name":"ci","cloudInitNoCloud":{}}
			]}}},
		"status":{"printableStatus":"Running","ready":true}}`)
	otherVM := obj(t, `{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachine","metadata":{"name":"other","namespace":"team-a"},
		"spec":{"runStrategy":"Halted"},"status":{"printableStatus":"Stopped"}}`)
	stoppedVM := obj(t, `{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachine","metadata":{"name":"stopped","namespace":"team-a"},
		"spec":{"runStrategy":"Halted"},"status":{"printableStatus":"Stopped"}}`)
	webVMI := obj(t, `{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachineInstance","metadata":{"name":"web","namespace":"team-a"},
		"status":{"nodeName":"node-1","guestOSInfo":{"prettyName":"Fedora Linux 42"},"interfaces":[{"ipAddress":"10.0.0.5"}]}}`)
	otherVMI := obj(t, `{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachineInstance","metadata":{"name":"other","namespace":"team-a"},
		"status":{"nodeName":"node-2","interfaces":[{"ipAddress":"10.0.0.9"}]}}`)
	webRootDV := obj(t, `{"apiVersion":"cdi.kubevirt.io/v1beta1","kind":"DataVolume","metadata":{"name":"web-root","namespace":"team-a"},
		"status":{"phase":"Succeeded"}}`)
	webDataDV := obj(t, `{"apiVersion":"cdi.kubevirt.io/v1beta1","kind":"DataVolume","metadata":{"name":"web-data","namespace":"team-a"},
		"status":{"phase":"ImportInProgress","progress":"50.0%"}}`)
	otherRootDV := obj(t, `{"apiVersion":"cdi.kubevirt.io/v1beta1","kind":"DataVolume","metadata":{"name":"other-root","namespace":"team-a"},
		"status":{"phase":"Succeeded"}}`)

	dyn := newFakeDynamic(webVM, otherVM, stoppedVM, webVMI, otherVMI, webRootDV, webDataDV, otherRootDV)
	ctx := context.Background()

	all, err := List(ctx, dyn, "team-a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var wantWeb Summary
	for _, s := range all {
		if s.Name == "web" {
			wantWeb = s
		}
	}

	dyn.ClearActions()
	gotWeb, err := Get(ctx, dyn, "team-a", "web")
	if err != nil {
		t.Fatalf("Get web: %v", err)
	}
	if !reflect.DeepEqual(gotWeb, wantWeb) {
		t.Fatalf("Get summary = %+v, want %+v", gotWeb, wantWeb)
	}

	var gotGets []string
	for _, a := range dyn.Actions() {
		if a.GetVerb() == "list" {
			t.Fatalf("unexpected list action during Get: %s %s", a.GetVerb(), a.GetResource().Resource)
		}
		if ga, ok := a.(k8stesting.GetAction); ok {
			gotGets = append(gotGets, a.GetResource().Resource+"/"+ga.GetName())
		}
	}
	wantGets := []string{
		"virtualmachines/web",
		"virtualmachineinstances/web",
		"datavolumes/web-root",
		"datavolumes/web-data",
		"datavolumes/web-missing",
	}
	if !reflect.DeepEqual(gotGets, wantGets) {
		t.Fatalf("Get actions = %v, want %v", gotGets, wantGets)
	}

	// Stopped VM without VMI or DataVolumes ignores 404 VMI and makes no DV calls.
	dyn.ClearActions()
	gotStopped, err := Get(ctx, dyn, "team-a", "stopped")
	if err != nil {
		t.Fatalf("Get stopped: %v", err)
	}
	if gotStopped.Status != "Stopped" || gotStopped.Node != "" || len(gotStopped.Disks) != 0 {
		t.Fatalf("stopped summary: %+v", gotStopped)
	}
	var stoppedGets []string
	for _, a := range dyn.Actions() {
		if a.GetVerb() == "list" {
			t.Fatalf("unexpected list action: %v", a)
		}
		if ga, ok := a.(k8stesting.GetAction); ok {
			stoppedGets = append(stoppedGets, a.GetResource().Resource+"/"+ga.GetName())
		}
	}
	if !reflect.DeepEqual(stoppedGets, []string{"virtualmachines/stopped", "virtualmachineinstances/stopped"}) {
		t.Fatalf("stopped Get actions = %v", stoppedGets)
	}

	// Missing VM returns NotFound without extra calls.
	dyn.ClearActions()
	if _, err := Get(ctx, dyn, "team-a", "nonexistent"); !apierrors.IsNotFound(err) {
		t.Fatalf("Get nonexistent err = %v, want NotFound", err)
	}
	if len(dyn.Actions()) != 1 {
		t.Fatalf("expected 1 action for nonexistent VM, got %d", len(dyn.Actions()))
	}
}
