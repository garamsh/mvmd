//go:build e2e_real

package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// A Machine created with a working directory runs its entrypoint there, at the first boot and after
// a Stop; one created without runs in the image's.
func TestE2E_CreateWorkingDirectoryIsWhereTheEntrypointRuns(t *testing.T) {
	n := startNode(t)
	n.arm(t)
	pulled := n.pullEntryIn(t, "workdir", "/image-dir", []string{"pwd"}, layer(dir("image-dir"), dir("srv"), dir("srv/app")))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	req := createRequest("wd", pulled.Digest)
	req.WorkingDir = "/srv/app"
	n.runMachine(t, ctx, req)
	assert.Equal(t, "/srv/app\n", n.entrypointOutput(t, ctx, "wd"))

	inspected, err := n.client.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "wd"})
	require.NoError(t, err)
	assert.Equal(t, "/srv/app", inspected.Machine.WorkingDir)

	_, err = n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: "wd", GracePeriod: durationpb.New(10 * time.Second)})
	require.NoError(t, err)
	_, err = n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "wd"})
	require.NoError(t, err)
	assert.Equal(t, "/srv/app\n", n.entrypointOutput(t, ctx, "wd"), "the boot after a Stop")

	// Control: none given runs in the image's.
	n.runMachine(t, ctx, createRequest("wd-image", pulled.Digest))
	assert.Equal(t, "/image-dir\n", n.entrypointOutput(t, ctx, "wd-image"))

	// A working directory that is not absolute and clean is refused with the path named.
	bad := createRequest("wd-bad", pulled.Digest)
	bad.WorkingDir = "srv/app"
	_, err = n.client.CreateMachine(ctx, bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "srv/app")
}

// A stopped Machine started with a run update boots with the env and files of the update in place
// of the record's, and a plain start after it keeps them. A refused update changes nothing, and no
// refusal, call or log of the daemon carries a value or a content.
func TestE2E_StartWithRunUpdateReplacesEnvAndFiles(t *testing.T) {
	n := startNode(t)
	n.arm(t)
	pulled := n.pullEntry(t, "runupdate", []string{"probe", "TOKEN", "LATE"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	const old = "OLD-VALUE-77aa03"
	req := createRequest("ru", pulled.Digest)
	req.Env = []*v1alpha2.EnvVar{{Name: "TOKEN", Value: old}}
	req.Files = []*v1alpha2.File{{Path: "/etc/app/key", Content: []byte(old), Mode: 0o600}}
	n.runMachine(t, ctx, req)
	assert.Contains(t, n.entrypointOutput(t, ctx, "ru"), " TOKEN="+old+" ")
	out, _ := n.exec(t, ctx, "ru", "cat-old", "/bin/entry", "cat", "/etc/app/key")
	require.Equal(t, old, out, "the setup: the record's file is in the guest")

	stop := func() {
		t.Helper()
		_, err := n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: "ru", GracePeriod: durationpb.New(10 * time.Second)})
		require.NoError(t, err)
	}
	stop()

	// A refused update changes nothing: the Machine stays Stopped and the next plain start boots
	// with the record's.
	_, err := n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "ru", RunUpdate: &v1alpha2.RunUpdate{
		Env:   []*v1alpha2.EnvVar{{Name: "TOKEN", Value: secret}},
		Files: []*v1alpha2.File{{Path: "/proc/x", Content: []byte(secret)}},
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/proc/x")
	assert.NotContains(t, err.Error(), secret)

	_, err = n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "ru"})
	require.NoError(t, err)
	assert.Contains(t, n.entrypointOutput(t, ctx, "ru"), " TOKEN="+old+" ", "no run_update, after a refusal: the record's")
	stop()

	// The update replaces both lists: TOKEN is the new value, LATE is new, the old file is gone.
	started, err := n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "ru", RunUpdate: &v1alpha2.RunUpdate{
		Env:   []*v1alpha2.EnvVar{{Name: "TOKEN", Value: secret}, {Name: "LATE", Value: "late"}},
		Files: []*v1alpha2.File{{Path: "/etc/app/new", Content: []byte(secret), Mode: 0o640}},
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{"TOKEN", "LATE"}, started.Machine.EnvNames)
	line := n.entrypointOutput(t, ctx, "ru")
	assert.Contains(t, line, " TOKEN="+secret+" ")
	assert.Contains(t, line, " LATE=late")
	out, _ = n.exec(t, ctx, "ru", "cat-new", "/bin/entry", "cat", "/etc/app/new")
	assert.Equal(t, secret, out)
	_, code := n.exec(t, ctx, "ru", "cat-gone", "/bin/entry", "cat", "/etc/app/key")
	assert.NotZero(t, code, "the file the update did not list is gone")

	// A plain start after it boots with what the update left in the record.
	stop()
	_, err = n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "ru"})
	require.NoError(t, err)
	assert.Contains(t, n.entrypointOutput(t, ctx, "ru"), " TOKEN="+secret+" ")

	inspected, err := n.client.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "ru"})
	require.NoError(t, err)
	wire, err := protojson.Marshal(inspected)
	require.NoError(t, err)
	assert.NotContains(t, string(wire), secret, "no value and no content in Inspect")
	assert.NotContains(t, n.log(), secret, "no value and no content in the daemon's log")
}
