package vm

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"

	"kvui/internal/image"
)

// Ref names an instancetype or preference, cluster-wide or namespaced.
type Ref struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Disk sources offered by the create form.
const (
	SourceRegistry      = "registry"      // CDI imports a container image into a PVC
	SourceHTTP          = "http"          // CDI downloads a qcow2/raw/iso image into a PVC
	SourceContainerDisk = "containerdisk" // ephemeral; changes are lost on restart
	SourceBlank         = "blank"         // empty PVC
	SourceImage         = "image"         // clone of an uploaded disk image
)

// CreateRequest is what the create form submits. Exactly one of
// Instancetype or CPUs/Memory sets the VM's size.
type CreateRequest struct {
	Name         string   `json:"name"`
	Instancetype *Ref     `json:"instancetype,omitempty"`
	CPUs         int64    `json:"cpus,omitempty"`
	Memory       string   `json:"memory,omitempty"`
	Preference   *Ref     `json:"preference,omitempty"`
	DiskSource   string   `json:"diskSource"`
	DiskURL      string   `json:"diskUrl,omitempty"`
	Image        string   `json:"image,omitempty"` // uploaded disk image to clone (SourceImage)
	CDROM        string   `json:"cdrom,omitempty"` // uploaded ISO to attach as a CD-ROM
	DiskSize     string   `json:"diskSize,omitempty"`
	StorageClass string   `json:"storageClass,omitempty"`
	SSHKeys      []string `json:"sshKeys,omitempty"`
	UserData     string   `json:"userData,omitempty"` // replaces the generated cloud-config
	Start        bool     `json:"start"`
}

// ValidationError is a problem with the request the user can fix.
type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

func invalid(format string, args ...any) error {
	return &ValidationError{msg: fmt.Sprintf(format, args...)}
}

const (
	maxCPUs     = 128
	maxUserData = 16 << 10
)

var (
	minMemory = resource.MustParse("64Mi")
	maxMemory = resource.MustParse("1Ti")
	minDisk   = resource.MustParse("1Gi")

	instancetypeKinds = map[string]bool{"VirtualMachineClusterInstancetype": true, "VirtualMachineInstancetype": true}
	preferenceKinds   = map[string]bool{"VirtualMachineClusterPreference": true, "VirtualMachinePreference": true}
)

// Validate checks the request and normalises it in place.
func (r *CreateRequest) Validate() error {
	r.Name = strings.TrimSpace(r.Name)
	if errs := validation.IsDNS1123Label(r.Name); len(errs) > 0 {
		return invalid("name: %s", strings.Join(errs, "; "))
	}

	if r.Instancetype != nil {
		if !instancetypeKinds[r.Instancetype.Kind] || r.Instancetype.Name == "" {
			return invalid("invalid instancetype")
		}
		if r.CPUs != 0 || r.Memory != "" {
			return invalid("choose an instancetype or a custom size, not both")
		}
	} else {
		if r.CPUs < 1 || r.CPUs > maxCPUs {
			return invalid("vCPUs must be between 1 and %d", maxCPUs)
		}
		mem, err := resource.ParseQuantity(r.Memory)
		if err != nil || mem.Cmp(minMemory) < 0 || mem.Cmp(maxMemory) > 0 {
			return invalid("memory must be a size between 64Mi and 1Ti, e.g. 4Gi")
		}
		r.Memory = mem.String()
	}
	if r.Preference != nil && (!preferenceKinds[r.Preference.Kind] || r.Preference.Name == "") {
		return invalid("invalid preference")
	}

	r.DiskURL = strings.TrimSpace(r.DiskURL)
	switch r.DiskSource {
	case SourceRegistry, SourceContainerDisk:
		if r.DiskURL == "" || strings.ContainsAny(r.DiskURL, " \t\n") {
			return invalid("enter a container image, e.g. quay.io/containerdisks/fedora:latest")
		}
		r.DiskURL = strings.TrimPrefix(r.DiskURL, "docker://")
	case SourceHTTP:
		if err := checkImportURL(r.DiskURL); err != nil {
			return err
		}
	case SourceBlank:
		r.DiskURL = ""
	case SourceImage:
		r.DiskURL = ""
		if len(validation.IsDNS1123Label(r.Image)) > 0 {
			return invalid("choose an uploaded disk image")
		}
	default:
		return invalid("unknown disk source %q", r.DiskSource)
	}
	if r.DiskSource != SourceImage {
		r.Image = ""
	}
	if r.CDROM != "" && len(validation.IsDNS1123Label(r.CDROM)) > 0 {
		return invalid("invalid CD-ROM image")
	}
	if r.DiskSource == SourceContainerDisk {
		r.DiskSize, r.StorageClass = "", ""
	} else {
		// Build names the root DataVolume "<name>-root", which must stay
		// within the 63-character DNS label limit.
		if len(r.Name) > 58 {
			return invalid("name must be at most 58 characters so its disk name fits within 63")
		}
		size, err := resource.ParseQuantity(r.DiskSize)
		if err != nil || size.Cmp(minDisk) < 0 {
			return invalid("disk size must be at least 1Gi, e.g. 20Gi")
		}
		r.DiskSize = size.String()
		if r.StorageClass != "" {
			if errs := validation.IsDNS1123Subdomain(r.StorageClass); len(errs) > 0 {
				return invalid("invalid storage class")
			}
		}
	}

	keys := r.SSHKeys[:0]
	for _, k := range r.SSHKeys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if strings.ContainsAny(k, "\r\n") || !looksLikeSSHKey(k) {
			return invalid("not an SSH public key: %.40s", k)
		}
		keys = append(keys, k)
	}
	r.SSHKeys = keys
	if len(r.UserData) > maxUserData {
		return invalid("user data is limited to 16 KiB")
	}
	return nil
}

func looksLikeSSHKey(k string) bool {
	for _, p := range []string{"ssh-", "ecdsa-sha2-", "sk-ssh-", "sk-ecdsa-"} {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// checkImportURL rejects URLs aimed at cluster-internal or local addresses.
// CDI's importer pod fetches the URL from inside the tenant namespace, so
// this is defence in depth only: a public hostname can still resolve to a
// private address, which is why tenant namespaces also need an egress
// NetworkPolicy for importer pods.
func checkImportURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return invalid("enter an http:// or https:// URL to a disk image")
	}
	// URL hosts may carry a zone ("%zone" for IPv6 literals); it only makes
	// sense for local addresses, so refuse it rather than parse around it.
	if strings.Contains(u.Host, "%") {
		return invalid("image URLs must point at a public host")
	}
	// A trailing dot makes "localhost." a different string but the same
	// host; strip it before comparing.
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if ip, err := netip.ParseAddr(host); err == nil {
		if !isPublicIP(ip) {
			return invalid("image URLs must point at a public host")
		}
		// Transition mechanisms embed an IPv4 address that is what will
		// actually be reached (IPv4-mapped, NAT64, 6to4); it must pass
		// the same test, since the outer address looks globally routable.
		if v4, ok := embeddedIPv4(ip); ok && !isPublicIP(v4) {
			return invalid("image URLs must point at a public host")
		}
		return nil
	}
	if host == "localhost" || strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".local") ||
		strings.Contains(host, ".svc.") || !strings.Contains(host, ".") {
		return invalid("image URLs must point at a public host")
	}
	// Shorthand and non-decimal IPv4 forms (127.1, 0177.0.0.1, 0x7f.1) do
	// not parse as addresses above, but other resolvers in the importer
	// may still read them as IPs. A final all-numeric (or 0x…) label is
	// the giveaway (RFC 3696 §2); no public host looks like that.
	if last := host[strings.LastIndex(host, ".")+1:]; isNumericLabel(last) {
		return invalid("image URLs must point at a public host")
	}
	return nil
}

var (
	cgnat      = netip.MustParsePrefix("100.64.0.0/10")
	zeroNet    = netip.MustParsePrefix("0.0.0.0/8") // "this host" range; Linux routes it to loopback
	nat64WK    = netip.MustParsePrefix("64:ff9b::/96")
	nat64Local = netip.MustParsePrefix("64:ff9b:1::/48")
	sixToFour  = netip.MustParsePrefix("2002::/16")
)

func isPublicIP(ip netip.Addr) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !cgnat.Contains(ip.Unmap()) && !zeroNet.Contains(ip.Unmap())
}

// embeddedIPv4 extracts the IPv4 address carried by an IPv4-mapped, NAT64
// (well-known or local-use prefix) or 6to4 address. The NAT64 extraction
// reads the last 32 bits, which is where a /96 prefix puts the address;
// the well-known prefix is always /96, and the local-use prefix is most
// commonly deployed that way too, but RFC 6052 allows other prefix
// lengths that place the address elsewhere. Those are not decoded — this
// check is defence in depth behind the importer NetworkPolicy, not the
// boundary itself.
func embeddedIPv4(ip netip.Addr) (netip.Addr, bool) {
	if ip.Is4() {
		return netip.Addr{}, false
	}
	if ip.Is4In6() {
		return ip.Unmap(), true
	}
	b := ip.As16()
	switch {
	case nat64WK.Contains(ip), nat64Local.Contains(ip):
		return netip.AddrFrom4([4]byte(b[12:])), true
	case sixToFour.Contains(ip):
		return netip.AddrFrom4([4]byte(b[2:6])), true
	}
	return netip.Addr{}, false
}

func isNumericLabel(s string) bool {
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "0x") {
		return true
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// CloudConfig returns the cloud-init user data for r: the user's own, or a
// minimal cloud-config installing their SSH keys for the image's default user.
func (r *CreateRequest) CloudConfig() string {
	if strings.TrimSpace(r.UserData) != "" {
		return r.UserData
	}
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	if len(r.SSHKeys) > 0 {
		b.WriteString("ssh_authorized_keys:\n")
		for _, k := range r.SSHKeys {
			fmt.Fprintf(&b, "  - %q\n", k)
		}
	}
	return b.String()
}

// Build returns the VirtualMachine object for a validated request.
func (r *CreateRequest) Build(namespace, createdBy string) *unstructured.Unstructured {
	rootDisk := map[string]any{"name": "root", "disk": map[string]any{"bus": "virtio"}}
	cloudInitDisk := map[string]any{"name": "cloudinit", "disk": map[string]any{"bus": "virtio"}}
	if r.Preference != nil {
		// Let the preference choose buses (e.g. sata for Windows).
		rootDisk["disk"], cloudInitDisk["disk"] = map[string]any{}, map[string]any{}
	}

	disks := []any{rootDisk, cloudInitDisk}
	if r.CDROM != "" {
		// Disk first, then CD-ROM: a blank disk falls through to the
		// installer, and once installed the VM boots from its disk.
		rootDisk["bootOrder"] = int64(1)
		disks = append(disks, map[string]any{"name": "cdrom", "bootOrder": int64(2), "cdrom": map[string]any{"bus": "sata"}})
	}
	domain := map[string]any{
		"devices": map[string]any{
			"disks":      disks,
			"interfaces": []any{map[string]any{"name": "default", "masquerade": map[string]any{}}},
		},
	}
	if r.Instancetype == nil {
		domain["cpu"] = map[string]any{"cores": r.CPUs}
		domain["memory"] = map[string]any{"guest": r.Memory}
	}

	var rootVolume map[string]any
	spec := map[string]any{}
	if r.DiskSource == SourceContainerDisk {
		rootVolume = map[string]any{"name": "root", "containerDisk": map[string]any{"image": r.DiskURL}}
	} else {
		dvName := r.Name + "-root"
		var source map[string]any
		switch r.DiskSource {
		case SourceRegistry:
			source = map[string]any{"registry": map[string]any{"url": "docker://" + r.DiskURL}}
		case SourceHTTP:
			source = map[string]any{"http": map[string]any{"url": r.DiskURL}}
		case SourceImage:
			source = map[string]any{"pvc": map[string]any{"namespace": namespace, "name": r.Image}}
		default:
			source = map[string]any{"blank": map[string]any{}}
		}
		storage := map[string]any{"resources": map[string]any{"requests": map[string]any{"storage": r.DiskSize}}}
		if r.StorageClass != "" {
			storage["storageClassName"] = r.StorageClass
		}
		spec["dataVolumeTemplates"] = []any{map[string]any{
			"metadata": map[string]any{"name": dvName},
			"spec":     map[string]any{"source": source, "storage": storage},
		}}
		rootVolume = map[string]any{"name": "root", "dataVolume": map[string]any{"name": dvName}}
	}

	runStrategy := "Halted"
	if r.Start {
		runStrategy = "Always"
	}
	spec["runStrategy"] = runStrategy
	if r.Instancetype != nil {
		spec["instancetype"] = map[string]any{"name": r.Instancetype.Name, "kind": r.Instancetype.Kind}
	}
	if r.Preference != nil {
		spec["preference"] = map[string]any{"name": r.Preference.Name, "kind": r.Preference.Kind}
	}
	volumes := []any{
		rootVolume,
		map[string]any{"name": "cloudinit", "cloudInitNoCloud": map[string]any{"userData": r.CloudConfig()}},
	}
	if r.CDROM != "" {
		volumes = append(volumes, map[string]any{"name": "cdrom", "dataVolume": map[string]any{"name": r.CDROM}})
	}
	spec["template"] = map[string]any{
		"metadata": map[string]any{"labels": map[string]any{"kubevirt.io/domain": r.Name}},
		"spec": map[string]any{
			"domain":   domain,
			"networks": []any{map[string]any{"name": "default", "pod": map[string]any{}}},
			"volumes":  volumes,
		},
	}

	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata": map[string]any{
			"name":        r.Name,
			"namespace":   namespace,
			"annotations": map[string]any{"kubevirt-ui.io/created-by": createdBy},
		},
		"spec": spec,
	}}
}

// Create validates and creates the VM as the caller's client.
func Create(ctx context.Context, dyn dynamic.Interface, namespace, createdBy string, r *CreateRequest) error {
	if err := r.Validate(); err != nil {
		return err
	}
	// Uploaded images must exist in the namespace and be the right kind.
	for _, use := range []struct{ name, want, field string }{{r.Image, image.TypeDisk, "disk image"}, {r.CDROM, image.TypeISO, "CD-ROM"}} {
		if use.name == "" {
			continue
		}
		typ, err := image.Type(ctx, dyn, namespace, use.name)
		if apierrors.IsNotFound(err) || image.IsValidation(err) || (err == nil && typ != use.want) {
			return invalid("%s: %q is not an uploaded %s image", use.field, use.name, use.want)
		}
		if err != nil {
			return err
		}
	}
	_, err := dyn.Resource(VMs).Namespace(namespace).Create(ctx, r.Build(namespace, createdBy), metav1.CreateOptions{})
	return err
}

// Delete removes a VM. Disks created from its dataVolumeTemplates are owned
// by the VM and garbage collected with it.
func Delete(ctx context.Context, dyn dynamic.Interface, namespace, name string) error {
	return dyn.Resource(VMs).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// IsValidation reports whether err is a user-fixable request problem.
func IsValidation(err error) bool {
	var v *ValidationError
	return errors.As(err, &v)
}
