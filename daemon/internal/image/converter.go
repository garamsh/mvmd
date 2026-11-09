package image

import (
	"context"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// ConverterPort is the port the converter listens on in its microVM
// (image.md §Conversion), the one a caller of Driver.GuestConn names for it.
const ConverterPort uint32 = 1025

// StagedLayer is a verified layer in staging.
type StagedLayer struct {
	Path      string
	MediaType string
}

// ConversionJob is what the converter is asked to do: the layers in manifest
// order, the UUID its EROFS carries, and the most bytes of entry names it
// holds for one layer.
type ConversionJob struct {
	FilesystemUUID string
	Layers         []StagedLayer
	NameSetBytes   uint64
}

// Converter is the conversation with the converter in its microVM (ADR 0017).
type Converter interface {
	// Convert has the converter of the booted VM of the given identifier
	// convert the layers into the EROFS on its output device, and returns the
	// EROFS's length. A conversion the converter reports as failed is an error
	// naming its step.
	Convert(ctx context.Context, id machine.MachineID, job ConversionJob) (length uint64, err error)
}
