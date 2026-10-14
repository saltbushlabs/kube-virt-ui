package vm

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func valid() CreateRequest {
	return CreateRequest{Name: "web", CPUs: 2, Memory: "4096Mi", DiskSource: SourceRegistry,
		DiskURL: "docker://quay.io/containerdisks/fedora:latest", DiskSize: "20Gi"}
}

func TestValidate(t *testing.T) {
	cases := map[string]func(*CreateRequest){
		"bad name": func(r *CreateRequest) { r.Name = "Web_1" },
		"both sizes": func(r *CreateRequest) {
			r.Instancetype = &Ref{Name: "u1.small", Kind: "VirtualMachineClusterInstancetype"}
		},
		"no size":                func(r *CreateRequest) { r.CPUs, r.Memory = 0, "" },
		"tiny memory":            func(r *CreateRequest) { r.Memory = "1Mi" },
		"bad kind":               func(r *CreateRequest) { r.CPUs, r.Memory, r.Instancetype = 0, "", &Ref{Name: "x", Kind: "Pod"} },
		"no image":               func(r *CreateRequest) { r.DiskURL = "" },
		"small disk":             func(r *CreateRequest) { r.DiskSize = "100Mi" },
		"private http":           func(r *CreateRequest) { r.DiskSource, r.DiskURL = SourceHTTP, "http://10.0.0.1/disk.img" },
		"metadata http":          func(r *CreateRequest) { r.DiskSource, r.DiskURL = SourceHTTP, "http://169.254.169.254/latest" },
		"service http":           func(r *CreateRequest) { r.DiskSource, r.DiskURL = SourceHTTP, "http://minio.storage.svc:9000/a.img" },
		"single label":           func(r *CreateRequest) { r.DiskSource, r.DiskURL = SourceHTTP, "http://minio/a.img" },
		"ftp":                    func(r *CreateRequest) { r.DiskSource, r.DiskURL = SourceHTTP, "ftp://example.com/a.img" },
		"trailing dot localhost": func(r *CreateRequest) { r.DiskSource, r.DiskURL = SourceHTTP, "http://localhost./a.img" },
		"trailing dot service":   func(r *CreateRequest) { r.DiskSource, r.DiskURL = SourceHTTP, "http://minio.storage.svc./a.img" },
		"trailing dot local":     func(r *CreateRequest) { r.DiskSource, r.DiskURL = SourceHTTP, "http://host.local./a.img" },
		"zero net http":          func(r *CreateRequest) { r.DiskSource, r.DiskURL = SourceHTTP, "http://0.0.0.1/a.img" },
		"cgnat http":             func(r *CreateRequest) { r.DiskSource, r.DiskURL = SourceHTTP, "http://100.64.0.1/a.img" },
		"multicast http":         func(r *CreateRequest) { r.DiskSource, r.DiskURL = SourceHTTP, "http://224.0.0.1/a.img" },
		"shorthand ipv4":         func(r *CreateRequest) { r.DiskSource, r.DiskURL = SourceHTTP, "http://127.1/a.img" },
		"octal ipv4":             func(r *CreateRequest) { r.DiskSource, r.DiskURL = SourceHTTP, "http://0177.0.0.1/a.img" },
		"hex ipv4":               func(r *CreateRequest) { r.DiskSource, r.DiskURL = SourceHTTP, "http://0x7f.1/a.img" },
		"ipv6 loopback":          func(r *CreateRequest) { r.DiskSource, r.DiskURL = SourceHTTP, "http://[::1]/a.img" },
		"ipv6 zone":              func(r *CreateRequest) { r.DiskSource, r.DiskURL = SourceHTTP, "http://[fe80::1%25eth0]/a.img" },
		"ipv4-mapped loopback":   func(r *CreateRequest) { r.DiskSource, r.DiskURL = SourceHTTP, "http://[::ffff:127.0.0.1]/a.img" },
		"ipv4-mapped private":    func(r *CreateRequest) { r.DiskSource, r.DiskURL = SourceHTTP, "http://[::ffff:10.0.0.1]/a.img" },
		"nat64 loopback":         func(r *CreateRequest) { r.DiskSource, r.DiskURL = SourceHTTP, "http://[64:ff9b::7f00:1]/a.img" },
		"nat64 private":          func(r *CreateRequest) { r.DiskSource, r.DiskURL = SourceHTTP, "http://[64:ff9b:1::a00:1]/a.img" },
		"6to4 loopback":          func(r *CreateRequest) { r.DiskSource, r.DiskURL = SourceHTTP, "http://[2002:7f00:1::1]/a.img" },
		"long name with dv":      func(r *CreateRequest) { r.Name = strings.Repeat("a", 59) },
		"bad ssh key":            func(r *CreateRequest) { r.SSHKeys = []string{"hello"} },
		"multiline key":          func(r *CreateRequest) { r.SSHKeys = []string{"ssh-ed25519 AAAA\nruncmd"} },
		"huge user data":         func(r *CreateRequest) { r.UserData = strings.Repeat("x", 17<<10) },
		"unknown source":         func(r *CreateRequest) { r.DiskSource = "nfs" },
	}
	for name, mutate := range cases {
		r := valid()
		mutate(&r)
		if err := r.Validate(); !IsValidation(err) {
			t.Errorf("%s: want validation error, got %v", name, err)
		}
	}
	r := valid()
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if r.Memory != "4Gi" || r.DiskURL != "quay.io/containerdisks/fedora:latest" {
		t.Fatalf("not normalised: %+v", r)
	}
	h := valid()
	h.DiskSource, h.DiskURL = SourceHTTP, "https://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img"
	if err := h.Validate(); err != nil {
		t.Fatalf("public http rejected: %v", err)
	}
	// Public IP literals, FQDN trailing dots and digit-leading labels are fine.
	for _, u := range []string{"https://8.8.8.8/a.img", "https://example.com./a.img", "https://3.example.com/a.img",
		"https://[::ffff:808:808]/a.img", "https://[64:ff9b::808:808]/a.img", "https://[2002:808:808::1]/a.img"} {
		h := valid()
		h.DiskSource, h.DiskURL = SourceHTTP, u
		if err := h.Validate(); err != nil {
			t.Errorf("%s rejected: %v", u, err)
		}
	}
	// The 58-char cap is about the "-root" DataVolume; container disks
	// have none, so full-length names stay valid for them.
	cd := valid()
	cd.Name = strings.Repeat("a", 63)
	cd.DiskSource = SourceContainerDisk
	if err := cd.Validate(); err != nil {
		t.Fatalf("63-char containerdisk name rejected: %v", err)
	}
	max := valid()
	max.Name = strings.Repeat("a", 58)
	if err := max.Validate(); err != nil {
		t.Fatalf("58-char name rejected: %v", err)
	}
}

func get(t *testing.T, u *unstructured.Unstructured, path ...string) any {
	t.Helper()
	v, ok, err := unstructured.NestedFieldNoCopy(u.Object, path...)
	if !ok || err != nil {
		return nil
	}
	return v
}

func TestBuildCustomRegistry(t *testing.T) {
	r := valid()
	r.SSHKeys = []string{"ssh-ed25519 AAAAC3Nza me@laptop", ""}
	r.Start = true
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	vm := r.Build("team-a", "alice")
	if get(t, vm, "spec", "runStrategy") != "Always" || get(t, vm, "spec", "instancetype") != nil {
		t.Fatal("run strategy / instancetype")
	}
	if get(t, vm, "spec", "template", "spec", "domain", "cpu", "cores") != int64(2) ||
		get(t, vm, "spec", "template", "spec", "domain", "memory", "guest") != "4Gi" {
		t.Fatal("custom size not set")
	}
	dvt := get(t, vm, "spec", "dataVolumeTemplates").([]any)[0].(map[string]any)
	if dvt["metadata"].(map[string]any)["name"] != "web-root" {
		t.Fatal("dv name")
	}
	src := dvt["spec"].(map[string]any)["source"].(map[string]any)["registry"].(map[string]any)["url"]
	if src != "docker://quay.io/containerdisks/fedora:latest" {
		t.Fatalf("registry url %v", src)
	}
	ci := r.CloudConfig()
	if !strings.HasPrefix(ci, "#cloud-config\n") || !strings.Contains(ci, `"ssh-ed25519 AAAAC3Nza me@laptop"`) {
		t.Fatalf("cloud-config: %q", ci)
	}
	if vm.GetAnnotations()["kubevirt-ui.io/created-by"] != "alice" {
		t.Fatal("created-by")
	}
}

func TestBuildInstancetypeContainerDisk(t *testing.T) {
	r := CreateRequest{Name: "it", DiskSource: SourceContainerDisk, DiskURL: "quay.io/containerdisks/fedora:latest", DiskSize: "5Gi",
		Instancetype: &Ref{Name: "u1.medium", Kind: "VirtualMachineClusterInstancetype"},
		Preference:   &Ref{Name: "fedora", Kind: "VirtualMachineClusterPreference"}}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	vm := r.Build("team-a", "alice")
	if get(t, vm, "spec", "template", "spec", "domain", "cpu") != nil || get(t, vm, "spec", "template", "spec", "domain", "memory") != nil {
		t.Fatal("instancetype VM must not set cpu/memory")
	}
	if get(t, vm, "spec", "dataVolumeTemplates") != nil || get(t, vm, "spec", "runStrategy") != "Halted" {
		t.Fatal("container disk should have no DataVolume and not start")
	}
	disk := get(t, vm, "spec", "template", "spec", "domain", "devices", "disks").([]any)[0].(map[string]any)
	if len(disk["disk"].(map[string]any)) != 0 {
		t.Fatal("bus should be left to the preference")
	}
}

func TestBuildImageCloneWithCDROM(t *testing.T) {
	r := CreateRequest{Name: "win", CPUs: 2, Memory: "4Gi", DiskSource: SourceImage, Image: "golden", DiskSize: "40Gi", CDROM: "virtio-win"}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	vm := r.Build("team-a", "alice")
	dvt := get(t, vm, "spec", "dataVolumeTemplates").([]any)[0].(map[string]any)
	pvc := dvt["spec"].(map[string]any)["source"].(map[string]any)["pvc"].(map[string]any)
	if pvc["name"] != "golden" || pvc["namespace"] != "team-a" {
		t.Fatalf("clone source %v", pvc)
	}
	disks := get(t, vm, "spec", "template", "spec", "domain", "devices", "disks").([]any)
	root, cdrom := disks[0].(map[string]any), disks[2].(map[string]any)
	if root["bootOrder"] != int64(1) || cdrom["bootOrder"] != int64(2) || cdrom["cdrom"].(map[string]any)["bus"] != "sata" {
		t.Fatalf("boot order: %v %v", root, cdrom)
	}
	vols := get(t, vm, "spec", "template", "spec", "volumes").([]any)
	if vols[2].(map[string]any)["dataVolume"].(map[string]any)["name"] != "virtio-win" {
		t.Fatalf("cdrom volume %v", vols[2])
	}

	r = CreateRequest{Name: "x", CPUs: 1, Memory: "1Gi", DiskSource: SourceImage, DiskSize: "10Gi"}
	if err := r.Validate(); !IsValidation(err) {
		t.Fatal("image source without image accepted")
	}
}
