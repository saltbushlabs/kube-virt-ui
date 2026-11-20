package vm

import (
	"context"
	"log/slog"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var (
	clusterInstancetypes = schema.GroupVersionResource{Group: "instancetype.kubevirt.io", Version: "v1beta1", Resource: "virtualmachineclusterinstancetypes"}
	instancetypes        = schema.GroupVersionResource{Group: "instancetype.kubevirt.io", Version: "v1beta1", Resource: "virtualmachineinstancetypes"}
	clusterPreferences   = schema.GroupVersionResource{Group: "instancetype.kubevirt.io", Version: "v1beta1", Resource: "virtualmachineclusterpreferences"}
	preferences          = schema.GroupVersionResource{Group: "instancetype.kubevirt.io", Version: "v1beta1", Resource: "virtualmachinepreferences"}
)

// Instancetype is a selectable VM size.
type Instancetype struct {
	Ref
	CPUs        int64  `json:"cpus"`
	Memory      string `json:"memory"`
	Description string `json:"description,omitempty"`
}

// Preference is a selectable set of guest OS defaults.
type Preference struct {
	Ref
	DisplayName string `json:"displayName,omitempty"`
}

// ListInstancetypes returns the cluster-wide and namespace instancetypes
// visible to the caller. A source the caller may not list is skipped.
func ListInstancetypes(ctx context.Context, dyn dynamic.Interface, namespace string) ([]Instancetype, error) {
	var out []Instancetype
	err := listBoth(ctx, dyn, namespace, clusterInstancetypes, instancetypes,
		"VirtualMachineClusterInstancetype", "VirtualMachineInstancetype",
		func(u *unstructured.Unstructured, kind string) {
			cpus, _, _ := unstructured.NestedInt64(u.Object, "spec", "cpu", "guest")
			mem, _, _ := unstructured.NestedString(u.Object, "spec", "memory", "guest")
			out = append(out, Instancetype{
				Ref: Ref{Name: u.GetName(), Kind: kind}, CPUs: cpus, Memory: mem,
				Description: u.GetAnnotations()["instancetype.kubevirt.io/description"],
			})
		})
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CPUs != out[j].CPUs {
			return out[i].CPUs < out[j].CPUs
		}
		return out[i].Name < out[j].Name
	})
	return out, err
}

// ListPreferences returns the cluster-wide and namespace preferences
// visible to the caller.
func ListPreferences(ctx context.Context, dyn dynamic.Interface, namespace string) ([]Preference, error) {
	var out []Preference
	err := listBoth(ctx, dyn, namespace, clusterPreferences, preferences,
		"VirtualMachineClusterPreference", "VirtualMachinePreference",
		func(u *unstructured.Unstructured, kind string) {
			out = append(out, Preference{
				Ref:         Ref{Name: u.GetName(), Kind: kind},
				DisplayName: u.GetAnnotations()["openshift.io/display-name"],
			})
		})
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, err
}

func listBoth(ctx context.Context, dyn dynamic.Interface, namespace string, cluster, namespaced schema.GroupVersionResource,
	clusterKind, namespacedKind string, add func(*unstructured.Unstructured, string)) error {
	sources := []struct {
		res          dynamic.ResourceInterface
		kind         string
		resource     string
		clusterScope bool
	}{
		{dyn.Resource(cluster), clusterKind, cluster.Resource, true},
		{dyn.Resource(namespaced).Namespace(namespace), namespacedKind, namespaced.Resource, false},
	}
	for _, s := range sources {
		list, err := s.res.List(ctx, metav1.ListOptions{})
		if apierrors.IsForbidden(err) {
			if s.clusterScope {
				slog.Warn("cannot list cluster-scoped resource; check the instancetype.kubevirt.io:view ClusterRoleBinding",
					"resource", s.resource, "namespace", namespace, "err", err)
			}
			continue
		}
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		for i := range list.Items {
			add(&list.Items[i], s.kind)
		}
	}
	return nil
}
