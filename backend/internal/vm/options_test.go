package vm

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

var optionsListKinds = map[schema.GroupVersionResource]string{
	clusterInstancetypes: "VirtualMachineClusterInstancetypeList",
	instancetypes:        "VirtualMachineInstancetypeList",
	clusterPreferences:   "VirtualMachineClusterPreferenceList",
	preferences:          "VirtualMachinePreferenceList",
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	clusterForbiddenWarned.Clear()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		clusterForbiddenWarned.Clear()
	})
	return &buf
}

func newOptionsClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), optionsListKinds, objs...)
}

func instancetypeObj(kind, namespace, name string, cpus int64, mem, desc string) *unstructured.Unstructured {
	meta := map[string]any{"name": name}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	if desc != "" {
		meta["annotations"] = map[string]any{"instancetype.kubevirt.io/description": desc}
	}
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "instancetype.kubevirt.io/v1beta1",
			"kind":       kind,
			"metadata":   meta,
			"spec": map[string]any{
				"cpu":    map[string]any{"guest": cpus},
				"memory": map[string]any{"guest": mem},
			},
		},
	}
}

func preferenceObj(kind, namespace, name, displayName string) *unstructured.Unstructured {
	meta := map[string]any{"name": name}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	if displayName != "" {
		meta["annotations"] = map[string]any{"openshift.io/display-name": displayName}
	}
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "instancetype.kubevirt.io/v1beta1",
			"kind":       kind,
			"metadata":   meta,
		},
	}
}

func TestListInstancetypes(t *testing.T) {
	buf := captureLogs(t)
	client := newOptionsClient(
		instancetypeObj("VirtualMachineClusterInstancetype", "", "u1.medium", 2, "4Gi", "General purpose"),
		instancetypeObj("VirtualMachineClusterInstancetype", "", "u1.small", 1, "2Gi", ""),
		instancetypeObj("VirtualMachineInstancetype", "team-a", "custom.a", 2, "8Gi", "Team custom"),
		instancetypeObj("VirtualMachineInstancetype", "team-b", "other.ns", 1, "1Gi", ""),
	)

	got, err := ListInstancetypes(context.Background(), client, "team-a")
	if err != nil {
		t.Fatalf("ListInstancetypes: %v", err)
	}
	want := []Instancetype{
		{Ref: Ref{Name: "u1.small", Kind: "VirtualMachineClusterInstancetype"}, CPUs: 1, Memory: "2Gi"},
		{Ref: Ref{Name: "custom.a", Kind: "VirtualMachineInstancetype"}, CPUs: 2, Memory: "8Gi", Description: "Team custom"},
		{Ref: Ref{Name: "u1.medium", Kind: "VirtualMachineClusterInstancetype"}, CPUs: 2, Memory: "4Gi", Description: "General purpose"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected logs: %s", buf.String())
	}
}

func TestListInstancetypesClusterForbiddenWarnsAndContinues(t *testing.T) {
	buf := captureLogs(t)
	client := newOptionsClient(
		instancetypeObj("VirtualMachineInstancetype", "team-a", "ns.small", 1, "2Gi", ""),
	)
	client.PrependReactor("list", clusterInstancetypes.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(clusterInstancetypes.GroupResource(), "", errors.New("not allowed"))
	})

	got, err := ListInstancetypes(context.Background(), client, "team-a")
	if err != nil {
		t.Fatalf("ListInstancetypes: %v", err)
	}
	want := []Instancetype{
		{Ref: Ref{Name: "ns.small", Kind: "VirtualMachineInstancetype"}, CPUs: 1, Memory: "2Gi"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	logs := buf.String()
	for _, sub := range []string{"level=WARN", "instancetype.kubevirt.io:view", clusterInstancetypes.Resource, "namespace=team-a"} {
		if !strings.Contains(logs, sub) {
			t.Errorf("warning log missing %q: %s", sub, logs)
		}
	}

	// Subsequent 403s for the same cluster-scoped resource log at DEBUG instead of repeating WARN.
	buf.Reset()
	if _, err := ListInstancetypes(context.Background(), client, "team-a"); err != nil {
		t.Fatalf("second ListInstancetypes: %v", err)
	}
	repeatLogs := buf.String()
	if strings.Contains(repeatLogs, "level=WARN") {
		t.Errorf("expected WARN only once per resource, got: %s", repeatLogs)
	}
	if !strings.Contains(repeatLogs, "level=DEBUG") || !strings.Contains(repeatLogs, clusterInstancetypes.Resource) {
		t.Errorf("expected DEBUG log on repeat 403, got: %s", repeatLogs)
	}
}

func TestListInstancetypesClusterNotFoundIsSilent(t *testing.T) {
	buf := captureLogs(t)
	client := newOptionsClient(
		instancetypeObj("VirtualMachineInstancetype", "team-a", "ns.small", 1, "2Gi", ""),
	)
	client.PrependReactor("list", clusterInstancetypes.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(clusterInstancetypes.GroupResource(), "")
	})

	got, err := ListInstancetypes(context.Background(), client, "team-a")
	if err != nil {
		t.Fatalf("ListInstancetypes: %v", err)
	}
	if len(got) != 1 || got[0].Name != "ns.small" {
		t.Fatalf("got %+v", got)
	}
	if buf.Len() != 0 {
		t.Errorf("404 NotFound should not log a warning, got: %s", buf.String())
	}
}

func TestListInstancetypesNamespacedForbiddenIsSilent(t *testing.T) {
	buf := captureLogs(t)
	client := newOptionsClient(
		instancetypeObj("VirtualMachineClusterInstancetype", "", "u1.small", 1, "2Gi", ""),
	)
	client.PrependReactor("list", instancetypes.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(instancetypes.GroupResource(), "", errors.New("not allowed"))
	})

	got, err := ListInstancetypes(context.Background(), client, "team-a")
	if err != nil {
		t.Fatalf("ListInstancetypes: %v", err)
	}
	if len(got) != 1 || got[0].Name != "u1.small" {
		t.Fatalf("got %+v", got)
	}
	if buf.Len() != 0 {
		t.Errorf("namespaced 403 should not emit cluster-scoped warning, got: %s", buf.String())
	}
}

func TestListInstancetypesUnexpectedError(t *testing.T) {
	buf := captureLogs(t)
	client := newOptionsClient()
	boom := apierrors.NewInternalError(errors.New("apiserver down"))
	client.PrependReactor("list", clusterInstancetypes.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, boom
	})

	if _, err := ListInstancetypes(context.Background(), client, "team-a"); !errors.Is(err, boom) {
		t.Fatalf("got err %v, want %v", err, boom)
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected logs: %s", buf.String())
	}
}

func TestListPreferences(t *testing.T) {
	buf := captureLogs(t)
	client := newOptionsClient(
		preferenceObj("VirtualMachineClusterPreference", "", "ubuntu", "Ubuntu"),
		preferenceObj("VirtualMachineClusterPreference", "", "fedora", "Fedora"),
		preferenceObj("VirtualMachinePreference", "team-a", "custom-os", "Custom OS"),
		preferenceObj("VirtualMachinePreference", "team-b", "other-os", "Other OS"),
	)

	got, err := ListPreferences(context.Background(), client, "team-a")
	if err != nil {
		t.Fatalf("ListPreferences: %v", err)
	}
	want := []Preference{
		{Ref: Ref{Name: "custom-os", Kind: "VirtualMachinePreference"}, DisplayName: "Custom OS"},
		{Ref: Ref{Name: "fedora", Kind: "VirtualMachineClusterPreference"}, DisplayName: "Fedora"},
		{Ref: Ref{Name: "ubuntu", Kind: "VirtualMachineClusterPreference"}, DisplayName: "Ubuntu"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected logs: %s", buf.String())
	}
}

func TestListPreferencesClusterForbiddenWarnsAndContinues(t *testing.T) {
	buf := captureLogs(t)
	client := newOptionsClient(
		preferenceObj("VirtualMachinePreference", "team-a", "custom-os", "Custom OS"),
	)
	client.PrependReactor("list", clusterPreferences.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(clusterPreferences.GroupResource(), "", errors.New("not allowed"))
	})

	got, err := ListPreferences(context.Background(), client, "team-a")
	if err != nil {
		t.Fatalf("ListPreferences: %v", err)
	}
	want := []Preference{
		{Ref: Ref{Name: "custom-os", Kind: "VirtualMachinePreference"}, DisplayName: "Custom OS"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	logs := buf.String()
	for _, sub := range []string{"level=WARN", "instancetype.kubevirt.io:view", clusterPreferences.Resource, "namespace=team-a"} {
		if !strings.Contains(logs, sub) {
			t.Errorf("warning log missing %q: %s", sub, logs)
		}
	}
}

func TestListPreferencesClusterNotFoundIsSilent(t *testing.T) {
	buf := captureLogs(t)
	client := newOptionsClient(
		preferenceObj("VirtualMachinePreference", "team-a", "custom-os", "Custom OS"),
	)
	client.PrependReactor("list", clusterPreferences.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(clusterPreferences.GroupResource(), "")
	})

	got, err := ListPreferences(context.Background(), client, "team-a")
	if err != nil {
		t.Fatalf("ListPreferences: %v", err)
	}
	if len(got) != 1 || got[0].Name != "custom-os" {
		t.Fatalf("got %+v", got)
	}
	if buf.Len() != 0 {
		t.Errorf("404 NotFound should not log a warning, got: %s", buf.String())
	}
}

func TestListPreferencesUnexpectedError(t *testing.T) {
	buf := captureLogs(t)
	client := newOptionsClient()
	boom := apierrors.NewInternalError(errors.New("apiserver down"))
	client.PrependReactor("list", preferences.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, boom
	})

	if _, err := ListPreferences(context.Background(), client, "team-a"); !errors.Is(err, boom) {
		t.Fatalf("got err %v, want %v", err, boom)
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected logs: %s", buf.String())
	}
}
