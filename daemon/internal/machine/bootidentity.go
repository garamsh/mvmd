package machine

// BootIdentity names the host's current boot. A Machine with RestartOnBoot
// records it at a start, and Restore compares it with the one recorded to tell
// a VMM that ended because the boot changed from one that ended in its boot
// (ADR 0062).
type BootIdentity interface {
	// BootID identifies the host's current boot; a different one means a reboot.
	BootID() string
}
