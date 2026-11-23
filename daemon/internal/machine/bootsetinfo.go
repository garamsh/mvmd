package machine

// BootSetOrigin is how a boot set came to be offered.
type BootSetOrigin string

const (
	// BootSetConfigured is a set the node's configuration names.
	BootSetConfigured BootSetOrigin = "configured"
	// BootSetAdded is a set a client added (ADR 0063).
	BootSetAdded BootSetOrigin = "added"
)

// BootSetInfo is a boot set the node offers and how it came to be offered.
type BootSetInfo struct {
	Name   string
	Set    BootSet
	Origin BootSetOrigin
}
