package v1alpha1

import (
	"fmt"
	"regexp"
	"strings"
)

var digestForm = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func validateDigest(digest string) error {
	if !digestForm.MatchString(digest) {
		return fmt.Errorf("image digest %q is not a sha256 digest", digest)
	}
	return nil
}

// Validate refuses an image reference whose repository carries a tag or an
// "@", or whose digest is not a sha256 digest.
func (r *ImageRef) Validate() error {
	repository := r.GetRepository()
	if repository == "" {
		return fmt.Errorf("image repository is empty")
	}
	if strings.Contains(repository, "@") {
		return fmt.Errorf("image repository %q carries a digest", repository)
	}
	if strings.Contains(repository[strings.LastIndex(repository, "/")+1:], ":") {
		return fmt.Errorf("image repository %q carries a tag", repository)
	}
	return validateDigest(r.GetDigest())
}

func requireIDs(ids map[string]string) error {
	for name, value := range ids {
		if value == "" {
			return fmt.Errorf("%s is empty", name)
		}
	}
	return nil
}

// Validate refuses a creating call without its identifier or request ID, or
// with an image that is not a sha256 digest.
func (r *CreateMachineRequest) Validate() error {
	if err := requireIDs(map[string]string{"machine_id": r.GetMachineId(), "request_id": r.GetRequestId()}); err != nil {
		return err
	}
	return validateDigest(r.GetImageDigest())
}

// Validate refuses a creating call without its identifier or request ID.
func (r *SnapshotMachineRequest) Validate() error {
	return requireIDs(map[string]string{"snapshot_id": r.GetSnapshotId(), "request_id": r.GetRequestId()})
}

// Validate refuses a creating call without its identifier or request ID.
func (r *RestoreMachineRequest) Validate() error {
	return requireIDs(map[string]string{"machine_id": r.GetMachineId(), "request_id": r.GetRequestId()})
}

// Validate refuses a creating call without its identifier or request ID.
func (r *ExecRequest) Validate() error {
	return requireIDs(map[string]string{"exec_id": r.GetExecId(), "request_id": r.GetRequestId()})
}

// Validate refuses an image reference that is not a sha256 digest.
func (r *PullImageRequest) Validate() error {
	return r.GetImage().Validate()
}
