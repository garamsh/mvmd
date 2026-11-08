package v1alpha1_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestImageRef_Validate(t *testing.T) {
	tests := []struct {
		name       string
		repository string
		digest     string
		valid      bool
	}{
		{"plain repository", "registry.example/ns/app", digest, true},
		{"host with a port is not a tag", "registry:5000/app", digest, true},
		{"tag on the last segment", "app:latest", digest, false},
		{"tag after a host with a port", "registry:5000/app:1.2", digest, false},
		{"digest inside the repository", "app@" + digest, digest, false},
		{"empty repository", "", digest, false},
		{"tag in place of the digest", "registry.example/app", "latest", false},
		{"uppercase digest", "registry.example/app", "sha256:ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := (&v1alpha1.ImageRef{Repository: tc.repository, Digest: tc.digest}).Validate()
			if tc.valid {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestCreateMachineRequest_Validate_RefusesNonDigestImage(t *testing.T) {
	ok := &v1alpha1.CreateMachineRequest{MachineId: "m", RequestId: "r", ImageDigest: digest}
	assert.NoError(t, ok.Validate())

	tagged := &v1alpha1.CreateMachineRequest{MachineId: "m", RequestId: "r", ImageDigest: "app:latest"}
	assert.Error(t, tagged.Validate())
}
