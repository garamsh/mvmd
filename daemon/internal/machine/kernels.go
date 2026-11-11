package machine

import "slices"

// KernelFile is a guest kernel the node offers: its path on the host and its
// SHA-256 digest in hex (ADR 0045).
type KernelFile struct {
	Path   string
	SHA256 string
}

// Kernels are the guest kernels a node offers, each by name, and the name a
// Machine created without one gets (ADR 0045).
type Kernels struct {
	Default string
	Offered map[string]KernelFile
}

// Names lists the offered names in order.
func (k Kernels) Names() []string {
	names := make([]string, 0, len(k.Offered))
	for name := range k.Offered {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// resolve returns the kernel a name selects, the empty name being the node's
// default.
func (k Kernels) resolve(name string) (string, KernelFile, bool) {
	if name == "" {
		name = k.Default
	}
	file, ok := k.Offered[name]
	return name, file, ok
}

// matching returns the name of an offered kernel with the digest. Of several
// it prefers the preferred name, then the default, then the first by name.
func (k Kernels) matching(digest, preferred string) (string, bool) {
	if digest == "" {
		return "", false
	}
	for _, name := range []string{preferred, k.Default} {
		if f, ok := k.Offered[name]; ok && f.SHA256 == digest {
			return name, true
		}
	}
	for _, name := range k.Names() {
		if k.Offered[name].SHA256 == digest {
			return name, true
		}
	}
	return "", false
}
