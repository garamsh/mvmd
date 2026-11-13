//go:build e2e_real

package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// A registry credential that expires is renewed on the running daemon: the credentials file is
// read at each lookup, so rewriting it is enough. The pulls span one daemon process.
func TestE2E_ARegistryCredentialThatExpiresIsRenewedWithoutRestartingTheDaemon(t *testing.T) {
	// Fixture strings made up here, never a real secret.
	const user, credentialA, credentialB = "e2e-node", "e2e-credential-a", "e2e-credential-b"
	n := startNodeWith(t, withRegistryCredential(user, credentialA))
	n.registry.requireBasic(user, credentialA)
	pid := n.cmd.Process.Pid
	first := n.registry.publish("renew-first", []string{"/bin/hello"}, layer(file("first", "x")))
	second := n.registry.publish("renew-second", []string{"/bin/hello"}, layer(file("second", "x")))

	// Control: a pull with A succeeds, and the registry was asking for a credential and took A.
	require.NoError(t, n.pullRef(first), "pull the first image with the credential the registry accepts")
	stats := n.registry.authStats()
	require.NotZero(t, stats.Challenged, "the registry challenged the daemon, so a credential was required")
	require.NotZero(t, stats.Accepted, "the registry served requests that carried A")
	require.Zero(t, stats.Refused, "the registry refused nothing while it accepted A")

	// Expiry: the registry accepts B only, and the file still holds A. The pull fails, and the
	// registry is the one that refused a credential the daemon sent.
	n.registry.requireBasic(user, credentialB)
	err := n.pullRef(second)
	require.Error(t, err, "pull the second image while the file holds the credential the registry stopped accepting")
	assert.Regexp(t, "(?i)401|unauthorized", err.Error(), "the failure is the registry's refusal")
	refused := n.registry.authStats().Refused
	require.NotZero(t, refused, "the registry refused a request that carried the credential the file held")

	// Renewal: the file holds B, and the same daemon pulls the same image.
	// The client may send the header it held once more and be refused before it looks the file up
	// again, so the refusals that follow are not counted; the registry took B.
	accepted := n.registry.authStats().Accepted
	n.writeRegistryCredential(user, credentialB)
	require.NoError(t, n.pullRef(second), "pull the second image after the file was rewritten with B")
	assert.Greater(t, n.registry.authStats().Accepted, accepted, "the registry served requests that carried B")

	// No restart: the process that served the first pull is running and is the one now.
	select {
	case exit := <-n.exited:
		t.Fatalf("the daemon exited during the pulls: %v", exit)
	default:
	}
	assert.Equal(t, pid, n.cmd.Process.Pid, "the daemon process is the one the first pull went to")
}

// pullRef pulls a published image through the runtime API and returns the call's error.
func (n *node) pullRef(r ref) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	_, err := n.client.PullImage(ctx, &v1alpha2.PullImageRequest{Image: &v1alpha2.ImageRef{Repository: r.Repository, Digest: r.Digest}})
	return err
}
