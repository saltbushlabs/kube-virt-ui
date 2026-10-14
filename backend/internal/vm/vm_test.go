package vm

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/json"
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
