package image

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// convert runs ADR 0007 step 6: it boots the converter VM, has it convert the
// job, and ends the VM, whatever the conversion did. The VM is a Machine of the
// conversion Driver and supervisor with its own user, one output disk and no
// network device, and its identifier names the image so that two pulls never
// share one. The user is released only once the VMM is known to have ended.
func (s *service) convert(ctx context.Context, image machine.ImageDigest, staging Staging, job ConversionJob) (uint64, error) {
	user, err := s.uids.AllocateConversion(image)
	if err != nil {
		return 0, fmt.Errorf("allocate the converter's user: %w", err)
	}
	id := conversionID(image)
	length, err := s.runConverter(ctx, id, user, staging, job)

	ctx, cancel := compensating(ctx)
	defer cancel()
	if ended := s.supervisor.Terminate(ctx, id); ended != nil {
		return 0, errors.Join(err, fmt.Errorf("end the converter's VMM: %w", ended))
	}
	s.uids.ReleaseConversion(image)
	return length, err
}

// runConverter boots the VM and converts, within the node's conversion limit.
func (s *service) runConverter(ctx context.Context, id machine.MachineID, user machine.User, staging Staging, job ConversionJob) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, s.settings.Limit)
	defer cancel()
	runDir, err := s.supervisor.RunDir(id)
	if err != nil {
		return 0, fmt.Errorf("name the converter's run directory: %w", err)
	}
	boot := machine.BootSpec{
		Devices:   []machine.BlockDevice{{Path: staging.Output}},
		Resources: s.settings.Resources,
		RunDir:    runDir,
	}
	process, err := s.driver.Describe(ctx, id, boot)
	if err != nil {
		return 0, fmt.Errorf("describe the converter's VMM process: %w", err)
	}
	if err := s.supervisor.Start(ctx, id, process.For(boot), user); err != nil {
		return 0, fmt.Errorf("start the converter's VMM process: %w", err)
	}
	if err := s.driver.Boot(ctx, id, boot); err != nil {
		return 0, fmt.Errorf("boot the converter: %w", err)
	}
	length, err := s.converter.Convert(ctx, id, job)
	if err != nil {
		return 0, fmt.Errorf("convert: %w", err)
	}
	return length, nil
}

// conversionID is the identifier of the VM converting the image.
func conversionID(image machine.ImageDigest) machine.MachineID {
	return machine.MachineID("convert-" + strings.TrimPrefix(string(image), "sha256:"))
}

// filesystemUUID is the UUID the image's EROFS carries, in the 8-4-4-4-12
// form, taken from the image's digest so that one image always carries the
// same one. The digest has been checked to be 64 hexadecimal digits.
func filesystemUUID(image machine.ImageDigest) string {
	h := strings.TrimPrefix(string(image), "sha256:")
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
