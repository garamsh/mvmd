// Package held is the nodecapacity.Reader that adds the Machines a node can
// hold and holds to the capacity another Reader measures (ADR 0046).
package held

import (
	"fmt"

	"github.com/garamsh/mvmd/daemon/internal/nodecapacity"
)

// Reader reads the measured capacity of the Reader it wraps and sets the
// Machine figures on it.
type Reader struct {
	measured nodecapacity.Reader
	limit    uint32
	count    func() (uint32, error)
}

var _ nodecapacity.Reader = (*Reader)(nil)

// New returns a Reader that reports limit as the most Machines the node can
// hold and what count returns as the Machines it holds, beside what measured
// reads.
func New(measured nodecapacity.Reader, limit uint32, count func() (uint32, error)) *Reader {
	return &Reader{measured: measured, limit: limit, count: count}
}

// Read implements nodecapacity.Reader. A count that cannot be read fails it.
func (r *Reader) Read() (nodecapacity.Capacity, error) {
	c, err := r.measured.Read()
	if err != nil {
		return nodecapacity.Capacity{}, err
	}
	n, err := r.count()
	if err != nil {
		return nodecapacity.Capacity{}, fmt.Errorf("count the Machines: %w", err)
	}
	c.MachineLimit, c.Machines = r.limit, n
	return c, nil
}
