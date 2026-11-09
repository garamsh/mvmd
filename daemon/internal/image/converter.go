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
	// EROFS's length. A conversion the converter reports as failed is a
	// *ConversionFailure naming its step.
	Convert(ctx context.Context, id machine.MachineID, job ConversionJob) (length uint64, err error)
}

// ConversionStep is the step of a conversion the converter reports as failed
// (ADR 0017), the one the pull branches on.
type ConversionStep string

const (
	StepStart       ConversionStep = "start"
	StepLayerType   ConversionStep = "layer type"
	StepStreamOrder ConversionStep = "stream order"
	StepNames       ConversionStep = "names"
	StepNameLimit   ConversionStep = "name limit"
	StepConvert     ConversionStep = "convert"
	StepOutput      ConversionStep = "output"
)

// ConversionFailure is a conversion the converter reported as failed: the
// step that failed and the converter's message, which is for people.
type ConversionFailure struct {
	Step    ConversionStep
	Message string
}

func (e *ConversionFailure) Error() string {
	return "the converter failed at step " + string(e.Step) + ": " + e.Message
}
