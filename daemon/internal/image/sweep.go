package image

import (
	"context"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func (s *service) Sweep(ctx context.Context) error {
	if err := s.store.Sweep(ctx); err != nil {
		return machine.NewError(machine.ErrFailed, "sweep the image cache: %v", err)
	}
	return nil
}
