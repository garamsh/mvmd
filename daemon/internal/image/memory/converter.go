package memory

import (
	"context"
	"slices"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	machinememory "github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// DefaultLength is the length a Converter reports unless told another.
const DefaultLength uint64 = 4096

// Converter is an in-memory image.Converter. Its step is "convert". It keeps
// the jobs it was given, and answers with the length it was told to report, or
// with what the function given to Serve returns.
type Converter struct {
	machinememory.Failures

	mu     sync.Mutex
	length uint64
	serve  func(ctx context.Context, id machine.MachineID, job image.ConversionJob) (uint64, error)
	jobs   []image.ConversionJob
}

// NewConverter returns a Converter that reports DefaultLength.
func NewConverter() *Converter {
	return &Converter{length: DefaultLength}
}

// Report sets the length later conversions report.
func (c *Converter) Report(length uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.length = length
}

// Serve makes later conversions run serve in place of reporting a length, as a
// converter that takes time or looks at its VM would.
func (c *Converter) Serve(serve func(ctx context.Context, id machine.MachineID, job image.ConversionJob) (uint64, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.serve = serve
}

// Jobs returns the jobs conversions were given, in the order they arrived.
func (c *Converter) Jobs() []image.ConversionJob {
	c.mu.Lock()
	defer c.mu.Unlock()
	jobs := slices.Clone(c.jobs)
	for i := range jobs {
		jobs[i].Layers = slices.Clone(jobs[i].Layers)
	}
	return jobs
}

// Convert implements image.Converter.
func (c *Converter) Convert(ctx context.Context, id machine.MachineID, job image.ConversionJob) (uint64, error) {
	c.mu.Lock()
	c.jobs = append(c.jobs, job)
	serve, length := c.serve, c.length
	c.mu.Unlock()
	if err := c.Enter("convert"); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if serve != nil {
		return serve(ctx, id, job)
	}
	return length, nil
}
