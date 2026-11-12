// Package image is Pull image in the inside of the mvmd daemon: the steps of
// ADR 0007 across the Registry, the Converter channel and the Image Store, the
// converter microVM booted and ended through the lifecycle's Driver and
// Supervision ports, and the start-up sweep. It names no registry, VMM or file
// technology; every crossing is a port declared here or in package machine.
package image

import "github.com/garamsh/mvmd/daemon/internal/machine"

// Reference names an image to pull: the repository to fetch it from and its
// digest, never a tag.
type Reference struct {
	Repository string
	Digest     machine.ImageDigest
}

// Platform is the operating system and CPU architecture the node runs images
// on, by which the Registry selects a manifest from an index.
type Platform struct {
	OS           string
	Architecture string
}

// Descriptor is a blob the Registry serves: its media type, digest and size,
// against which the Registry checks what it streams.
type Descriptor struct {
	MediaType string
	Digest    string
	Size      int64
}

// Manifest is what the Registry found for the node's platform: the image's
// config and its layers in manifest order.
type Manifest struct {
	Config Descriptor
	Layers []Descriptor
}

// Config is the part of an image's config the node keeps: what the image runs.
// Env entries are in the form NAME=value.
type Config struct {
	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkingDir string
	// User is the config's User, in the OCI form, or empty.
	User string
}

// Staging is one pull's working area in the Image Store, which the cache's
// index never lists until Publish.
type Staging struct {
	ID string
	// Output is the file the converter's VM writes the EROFS to, sparse and as
	// large as the node's maximum image size.
	Output string
}

// Record is what a published MachineImage says of itself: the digest it was
// pulled by, what it runs, and what the pull made of it.
type Record struct {
	Image        machine.ImageDigest
	Run          machine.ImageRun
	Architecture string
	// Converter names the converter the image came from.
	Converter string
	// UUID is the filesystem UUID chosen for the EROFS.
	UUID string
	// Verity is the fs-verity digest of the EROFS, as Seal measured it.
	Verity string
}
