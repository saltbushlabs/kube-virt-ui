// Package vm reads and controls KubeVirt virtual machines. All calls take a
// client built from the requesting user's credentials, so the apiserver
// enforces what they may see and do.
//
// KubeVirt objects are handled as unstructured data rather than through
// kubevirt.io/client-go, which pins its own Kubernetes library versions.
package vm

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var (
	VMs  = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}
	VMIs = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}
	DVs  = schema.GroupVersionResource{Group: "cdi.kubevirt.io", Version: "v1beta1", Resource: "datavolumes"}
)

// Summary is the UI's view of a VM merged with its running instance.
type Summary struct {
	Name         string    `json:"name"`
	Namespace    string    `json:"namespace"`
	Status       string    `json:"status"` // KubeVirt printableStatus: Running, Stopped, Paused, ...
	Ready        bool      `json:"ready"`
	RunStrategy  string    `json:"runStrategy"`
	Instancetype string    `json:"instancetype,omitempty"`
	CPUs         int64     `json:"cpus,omitempty"` // 0 when set by an instancetype
	Memory       string    `json:"memory,omitempty"`
	Node         string    `json:"node,omitempty"`
	IPs          []string  `json:"ips"`
	OS           string    `json:"os,omitempty"`
	Created      time.Time `json:"created"`
	Disks        []Disk    `json:"disks"`
}

// Disk is the provisioning state of a DataVolume backing the VM, so the UI
// can show import progress and why a VM is stuck in Provisioning.
type Disk struct {
	Name     string `json:"name"`
	Phase    string `json:"phase"`              // CDI phase: ImportInProgress, Succeeded, Failed, ...
	Progress string `json:"progress,omitempty"` // e.g. "45.20%"
	Problem  string `json:"problem,omitempty"`  // error reported by CDI
}

// WatchVersions holds the collection ResourceVersions returned by ListWithRVs
// so Watch can resume from that snapshot without re-receiving synthetic ADDED
// events for existing objects.
type WatchVersions struct {
	VMs  string
	VMIs string
	DVs  string
}

// List returns all VMs in namespace, sorted by name.
func List(ctx context.Context, dyn dynamic.Interface, namespace string) ([]Summary, error) {
	out, _, err := ListWithRVs(ctx, dyn, namespace)
	return out, err
}

// ListWithRVs returns all VMs in namespace, sorted by name, along with the
// list ResourceVersions for VMs, VMIs, and DataVolumes (empty when CDI is not
// installed).
func ListWithRVs(ctx context.Context, dyn dynamic.Interface, namespace string) ([]Summary, WatchVersions, error) {
	vms, err := dyn.Resource(VMs).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, WatchVersions{}, err
	}
	vmis, vmiRV, err := byName(ctx, dyn, VMIs, namespace)
	if err != nil {
		return nil, WatchVersions{}, err
	}
	dvs, dvRV, err := byName(ctx, dyn, DVs, namespace)
	if apierrors.IsNotFound(err) {
		dvs, dvRV, err = nil, "", nil // CDI not installed
	}
	if err != nil {
		return nil, WatchVersions{}, err
	}
	out := make([]Summary, 0, len(vms.Items))
	for i := range vms.Items {
		out = append(out, summarize(&vms.Items[i], vmis[vms.Items[i].GetName()], dvs))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, WatchVersions{
		VMs:  vms.GetResourceVersion(),
		VMIs: vmiRV,
		DVs:  dvRV,
	}, nil
}

func byName(ctx context.Context, dyn dynamic.Interface, res schema.GroupVersionResource, namespace string) (map[string]*unstructured.Unstructured, string, error) {
	list, err := dyn.Resource(res).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, "", err
	}
	m := make(map[string]*unstructured.Unstructured, len(list.Items))
	for i := range list.Items {
		m[list.Items[i].GetName()] = &list.Items[i]
	}
	return m, list.GetResourceVersion(), nil
}

// Get returns one VM's summary using targeted Get calls for the VM, its
// running instance (if any), and the DataVolumes it references.
func Get(ctx context.Context, dyn dynamic.Interface, namespace, name string) (Summary, error) {
	vmObj, err := dyn.Resource(VMs).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Summary{}, err
	}
	vmi, err := dyn.Resource(VMIs).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		vmi, err = nil, nil
	}
	if err != nil {
		return Summary{}, err
	}
	var dvs map[string]*unstructured.Unstructured
	if dvNames := dataVolumeNames(vmObj.Object); len(dvNames) > 0 {
		dvs = make(map[string]*unstructured.Unstructured, len(dvNames))
		for _, dvName := range dvNames {
			if _, seen := dvs[dvName]; seen {
				continue
			}
			dv, err := dyn.Resource(DVs).Namespace(namespace).Get(ctx, dvName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				dvs[dvName] = nil // DataVolume absent or CDI not installed
				continue
			}
			if err != nil {
				return Summary{}, err
			}
			dvs[dvName] = dv
		}
	}
	return summarize(vmObj, vmi, dvs), nil
}

func dataVolumeNames(obj map[string]any) []string {
	volumes, _, _ := unstructured.NestedSlice(obj, "spec", "template", "spec", "volumes")
	var names []string
	for _, v := range volumes {
		m, ok := v.(map[string]any)
		if !ok {
			continue // malformed volume entry; not a reason to fail the whole list
		}
		if name, _, _ := unstructured.NestedString(m, "dataVolume", "name"); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func summarize(vm, vmi *unstructured.Unstructured, dvs map[string]*unstructured.Unstructured) Summary {
	obj := vm.Object
	s := Summary{
		Name:      vm.GetName(),
		Namespace: vm.GetNamespace(),
		Created:   vm.GetCreationTimestamp().Time,
		IPs:       []string{},
		Disks:     []Disk{},
	}
	s.Status, _, _ = unstructured.NestedString(obj, "status", "printableStatus")
	s.Ready, _, _ = unstructured.NestedBool(obj, "status", "ready")
	s.RunStrategy, _, _ = unstructured.NestedString(obj, "spec", "runStrategy")
	if s.RunStrategy == "" {
		// Older VMs use the deprecated spec.running boolean.
		if running, ok, _ := unstructured.NestedBool(obj, "spec", "running"); ok && running {
			s.RunStrategy = "Always"
		} else {
			s.RunStrategy = "Halted"
		}
	}
	s.Instancetype, _, _ = unstructured.NestedString(obj, "spec", "instancetype", "name")

	domain := []string{"spec", "template", "spec", "domain"}
	if s.Instancetype == "" {
		s.CPUs = 1
		for _, f := range []string{"cores", "sockets", "threads"} {
			if v, ok, _ := unstructured.NestedInt64(obj, append(domain, "cpu", f)...); ok && v > 0 {
				s.CPUs *= v
			}
		}
		s.Memory = firstQuantity(obj,
			append(domain, "memory", "guest"),
			append(domain, "resources", "requests", "memory"))
	}

	// A stopped VM's instance may linger briefly while it is deleted; its
	// node and addresses are no longer meaningful.
	if vmi != nil && s.Status != "Stopped" {
		s.Node, _, _ = unstructured.NestedString(vmi.Object, "status", "nodeName")
		s.OS, _, _ = unstructured.NestedString(vmi.Object, "status", "guestOSInfo", "prettyName")
		ifaces, _, _ := unstructured.NestedSlice(vmi.Object, "status", "interfaces")
		for _, i := range ifaces {
			if m, ok := i.(map[string]any); ok {
				if ip, _ := m["ipAddress"].(string); ip != "" {
					s.IPs = append(s.IPs, ip)
				}
			}
		}
	}
	if s.Status == "" {
		s.Status = "Unknown"
	}

	for _, name := range dataVolumeNames(obj) {
		if dv := dvs[name]; dv != nil {
			s.Disks = append(s.Disks, diskStatus(dv))
		}
	}
	return s
}

func diskStatus(dv *unstructured.Unstructured) Disk {
	d := Disk{Name: dv.GetName()}
	d.Phase, _, _ = unstructured.NestedString(dv.Object, "status", "phase")
	d.Progress, _, _ = unstructured.NestedString(dv.Object, "status", "progress")
	if d.Progress == "N/A" {
		d.Progress = ""
	}
	conds, _, _ := unstructured.NestedSlice(dv.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		reason, _ := m["reason"].(string)
		msg, _ := m["message"].(string)
		if msg != "" && (strings.HasPrefix(reason, "Err") || reason == "Error" || reason == "CrashLoopBackOff" || d.Phase == "Failed") {
			d.Problem = msg
			break
		}
	}
	return d
}

func firstQuantity(obj map[string]any, paths ...[]string) string {
	for _, p := range paths {
		v, ok, _ := unstructured.NestedFieldNoCopy(obj, p...)
		if !ok {
			continue
		}
		if q, err := resource.ParseQuantity(fmt.Sprint(v)); err == nil {
			return q.String()
		}
	}
	return ""
}

// Actions maps UI power actions onto KubeVirt subresources.
var Actions = map[string]string{
	"start":   "virtualmachines/start",
	"stop":    "virtualmachines/stop",
	"restart": "virtualmachines/restart",
	"pause":   "virtualmachineinstances/pause",
	"unpause": "virtualmachineinstances/unpause",
}

// Do performs a power action through the subresources.kubevirt.io API.
// rc must be a REST client for the user (any group; an absolute path is used).
func Do(ctx context.Context, rc rest.Interface, namespace, name, action string) error {
	sub, ok := Actions[action]
	if !ok {
		return fmt.Errorf("unknown action %q", action)
	}
	resourceType, verb, _ := strings.Cut(sub, "/")
	return rc.Put().
		AbsPath("/apis/subresources.kubevirt.io/v1/namespaces", namespace, resourceType, name, verb).
		SetHeader("Content-Type", "application/json").
		Body([]byte("{}")).
		Do(ctx).Error()
}

// Watch signals on the returned channel whenever a VM, VM instance or
// DataVolume in namespace changes after rvs. The channel is closed when ctx
// ends or any watch is closed by the apiserver; callers then re-establish it.
func Watch(ctx context.Context, dyn dynamic.Interface, namespace string, rvs WatchVersions) (<-chan struct{}, error) {
	targets := []struct {
		res schema.GroupVersionResource
		rv  string
	}{
		{VMs, rvs.VMs},
		{VMIs, rvs.VMIs},
		{DVs, rvs.DVs},
	}
	var watches []watch.Interface
	for _, t := range targets {
		w, err := dyn.Resource(t.res).Namespace(namespace).Watch(ctx, metav1.ListOptions{ResourceVersion: t.rv})
		if t.res == DVs && apierrors.IsNotFound(err) {
			continue // CDI not installed
		}
		if err != nil {
			for _, w := range watches {
				w.Stop()
			}
			return nil, err
		}
		watches = append(watches, w)
	}

	out := make(chan struct{}, 1)
	stop := make(chan struct{})
	var once sync.Once
	var wg sync.WaitGroup
	for _, w := range watches {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer once.Do(func() { close(stop) }) // one watch ending ends them all
			defer w.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-stop:
					return
				case ev, ok := <-w.ResultChan():
					if !ok || ev.Type == watch.Error {
						return
					}
					select {
					case out <- struct{}{}:
					default: // a signal is already pending
					}
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out, nil
}
