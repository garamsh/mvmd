//go:build e2e_real

package e2e_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// secret stands for a secret: the value of the env var and the content of the file the tests below
// give a Machine.
const secret = "MARKER-VALUE-4f9c1e"

// accounts is a layer with the user "app" (1000:1000) in the group "staff" (50), which the image
// user "app" and the request's user "app" are resolved against.
func accounts() []byte {
	return layer(
		dir("etc"),
		file("etc/passwd", "root:x:0:0::/:/bin/false\napp:x:1000:1000::/:/bin/false\n"),
		file("etc/group", "root:x:0:\napp:x:1000:\nstaff:x:50:app\n"),
	)
}

// A Machine created with command, args, env, user and files boots with each of them: the entrypoint
// runs the command and the args, with the env, as the user and its groups, and each file is in the
// guest with the mode and the owner asked for, at boot and again at a boot after a Stop. No call and
// no log of the daemon carries a value or a content.
func TestE2E_CreateRunConfigurationReachesTheGuestOnEveryBoot(t *testing.T) {
	n := startNode(t)
	n.arm(t)
	pulled := n.pullEntry(t, "runconfig", nil, accounts())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	req := createRequest("rc", pulled.Digest)
	req.Command = []string{"/bin/entry", "probe"}
	req.Args = []string{"TOKEN", "ABSENT"}
	req.Env = []*v1alpha2.EnvVar{{Name: "TOKEN", Value: secret}}
	req.User = "app"
	req.Files = []*v1alpha2.File{
		{Path: "/etc/app/key", Content: []byte(secret), Mode: 0o600, Uid: 1000, Gid: 1000},
		{Path: "/srv/conf/app.conf", Content: []byte("plain"), Mode: 0o644},
	}
	created, err := n.client.CreateMachine(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, []string{"TOKEN"}, created.Machine.EnvNames)
	require.Len(t, created.Machine.Files, 2)

	atBoot := func(boot string) {
		_, err := n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "rc"})
		require.NoError(t, err)

		line := n.entrypointOutput(t, ctx, "rc")
		assert.Contains(t, line, "uid=1000 gid=1000 groups=", "the entrypoint runs as the user")
		assert.Contains(t, line, "50", "with the user's supplementary group")
		assert.Contains(t, line, " TOKEN="+secret+" ", "with the env, as the args named it")
		assert.True(t, strings.HasSuffix(line, " ABSENT=\n"), "a name the request did not set is empty: %q", line)

		out, code := n.exec(t, ctx, "rc", "stat-key-"+boot, "/bin/entry", "stat", "/etc/app/key")
		assert.Zero(t, code)
		assert.Equal(t, "mode=0600 uid=1000 gid=1000\n", out)
		out, _ = n.exec(t, ctx, "rc", "cat-key-"+boot, "/bin/entry", "cat", "/etc/app/key")
		assert.Equal(t, secret, out)
		out, _ = n.exec(t, ctx, "rc", "stat-conf-"+boot, "/bin/entry", "stat", "/srv/conf/app.conf")
		assert.Equal(t, "mode=0644 uid=0 gid=0\n", out)
		out, _ = n.exec(t, ctx, "rc", "cat-conf-"+boot, "/bin/entry", "cat", "/srv/conf/app.conf")
		assert.Equal(t, "plain", out)
	}
	atBoot("first")
	_, err = n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: "rc", GracePeriod: durationpb.New(10 * time.Second)})
	require.NoError(t, err)
	atBoot("second")

	inspected, err := n.client.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "rc"})
	require.NoError(t, err)
	wire, err := protojson.Marshal(inspected)
	require.NoError(t, err)
	assert.NotContains(t, string(wire), secret, "no value and no content in Inspect")
	require.Len(t, inspected.Machine.Files, 2)
	assert.Equal(t, uint64(len(secret)), inspected.Machine.Files[0].SizeBytes)
	assert.NotContains(t, n.log(), secret, "no value and no content in the daemon's log")
}

// A request that sets no user runs as the image's user, and one that sets a user runs as that.
func TestE2E_CreateWithoutAUserRunsAsTheImagesUser(t *testing.T) {
	n := startNode(t)
	n.arm(t)
	pulled := n.pullEntryAs(t, "imageuser", "app", []string{"probe"}, accounts())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	n.runMachine(t, ctx, createRequest("image-user", pulled.Digest))
	assert.Contains(t, n.entrypointOutput(t, ctx, "image-user"), "uid=1000 gid=1000 ")

	// Control: the same image with a user in the request runs as the request's.
	req := createRequest("request-user", pulled.Digest)
	req.User = "0:0"
	n.runMachine(t, ctx, req)
	assert.Contains(t, n.entrypointOutput(t, ctx, "request-user"), "uid=0 gid=0 ")
}

// Args alone replace the image's command and keep its entrypoint; a command replaces both.
func TestE2E_CreateArgsAloneKeepTheImagesEntrypoint(t *testing.T) {
	n := startNode(t)
	n.arm(t)
	pulled := n.pullEntry(t, "argsalone", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	req := createRequest("args", pulled.Digest)
	req.Args = []string{"probe", "HOME"}
	n.runMachine(t, ctx, req)
	assert.Contains(t, n.entrypointOutput(t, ctx, "args"), "uid=0 gid=0 ", "the image's entrypoint ran the args")

	// Control: neither set boots the image's own, which prints its greeting.
	n.runMachine(t, ctx, createRequest("neither", pulled.Digest))
	assert.Equal(t, "hello from the entrypoint\n", n.entrypointOutput(t, ctx, "neither"))
}

// Each refusal at Create is REJECTED and names the field and the path, and echoes no content or
// value; the same request without the fault is accepted.
func TestE2E_CreateRefusesAnInvalidRunConfiguration(t *testing.T) {
	n := startNode(t)
	pulled := n.pullBootable(t, "refusals")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	file := func(path string) *v1alpha2.File {
		return &v1alpha2.File{Path: path, Content: []byte(secret), Mode: 0o600}
	}
	many := make([]*v1alpha2.File, 65)
	for i := range many {
		many[i] = file(fmt.Sprintf("/data/f%d", i))
	}
	tests := []struct {
		name    string
		edit    func(*v1alpha2.CreateMachineRequest)
		mention string
	}{
		{"empty env name", func(r *v1alpha2.CreateMachineRequest) { r.Env = []*v1alpha2.EnvVar{{Value: secret}} }, "env"},
		{"env name with =", func(r *v1alpha2.CreateMachineRequest) { r.Env = []*v1alpha2.EnvVar{{Name: "A=B", Value: secret}} }, "A=B"},
		{"env name twice", func(r *v1alpha2.CreateMachineRequest) {
			r.Env = []*v1alpha2.EnvVar{{Name: "A", Value: secret}, {Name: "A", Value: secret}}
		}, "A"},
		{"relative path", func(r *v1alpha2.CreateMachineRequest) { r.Files = []*v1alpha2.File{file("etc/x")} }, "etc/x"},
		{"unclean path", func(r *v1alpha2.CreateMachineRequest) { r.Files = []*v1alpha2.File{file("/etc/../x")} }, "/etc/../x"},
		{"under /proc", func(r *v1alpha2.CreateMachineRequest) { r.Files = []*v1alpha2.File{file("/proc/x")} }, "/proc/x"},
		{"under /sys", func(r *v1alpha2.CreateMachineRequest) { r.Files = []*v1alpha2.File{file("/sys/x")} }, "/sys/x"},
		{"under /dev", func(r *v1alpha2.CreateMachineRequest) { r.Files = []*v1alpha2.File{file("/dev/x")} }, "/dev/x"},
		{"under /run/mvmd", func(r *v1alpha2.CreateMachineRequest) { r.Files = []*v1alpha2.File{file("/run/mvmd/x")} }, "/run/mvmd/x"},
		{"mode above 07777", func(r *v1alpha2.CreateMachineRequest) {
			r.Files = []*v1alpha2.File{{Path: "/etc/x", Content: []byte(secret), Mode: 0o10000}}
		}, "/etc/x"},
		{"path twice", func(r *v1alpha2.CreateMachineRequest) { r.Files = []*v1alpha2.File{file("/etc/x"), file("/etc/x")} }, "/etc/x"},
		{"65 files", func(r *v1alpha2.CreateMachineRequest) { r.Files = many }, "files"},
		{"over 1 MiB", func(r *v1alpha2.CreateMachineRequest) {
			r.Files = []*v1alpha2.File{{Path: "/etc/x", Content: []byte(strings.Repeat("a", 1<<20+1)), Mode: 0o600}}
		}, "files"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := createRequest("refused", pulled.Digest)
			tt.edit(req)
			_, err := n.client.CreateMachine(ctx, req)
			require.Error(t, err)
			assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err), err.Error())
			assert.Contains(t, err.Error(), tt.mention)
			assert.NotContains(t, err.Error(), secret)

			// Control: without the fault the request is accepted, which also shows the refusal left
			// no Machine behind.
			_, err = n.client.CreateMachine(ctx, createRequest("refused", pulled.Digest))
			require.NoError(t, err)
			_, err = n.client.DeleteMachine(ctx, &v1alpha2.DeleteMachineRequest{MachineId: "refused"})
			require.NoError(t, err)
		})
	}
}
