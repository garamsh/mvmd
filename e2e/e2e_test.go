//go:build e2e_real

package e2e_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

const failed = v1alpha2.OutcomeKind_OUTCOME_KIND_FAILED

// A pull through the runtime API converts the image in a converter microVM and publishes it to
// the cache, where the Machine calls find it.
func TestE2E_PullPublishesTheImageTheMachineCallsFind(t *testing.T) {
	n := startNode(t)
	pulled := n.pull(t, "hello", layer(dir("bin"), file("bin/hello", "hello"), file("etc-release", "e2e")))

	assert.ElementsMatch(t, []string{"bin/", "bin/hello=hello", "etc-release=e2e"}, tree(t, n.erofsOf(t, pulled.Digest)))

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := n.client.CreateMachine(ctx, createRequest("from-the-pull", pulled.Digest))
	require.NoError(t, err, "a Machine of the pulled image is created: the image is in the cache and passes its fs-verity check")

	// The control: an image no pull brought to the node is not one a Machine is created from.
	_, err = n.client.CreateMachine(ctx, createRequest("from-nowhere", "sha256:"+zeros))
	require.Error(t, err)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_NOT_FOUND, v1alpha2.KindOf(err))
}

// A Machine started from a pulled image runs the image's entrypoint, and the output the entrypoint
// wrote is what the runtime API reads back.
func TestE2E_AMachineStartsFromThePulledImageAndItsEntrypointSpeaks(t *testing.T) {
	n := startNode(t)
	n.arm(t)
	pulled := n.pullBootable(t, "bootable")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	_, err := n.client.CreateMachine(ctx, createRequest("boots", pulled.Digest))
	require.NoError(t, err)
	_, err = n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "boots"})
	require.NoError(t, err, "the Machine of the pulled image starts")

	assert.Equal(t, "hello from the entrypoint\n", n.entrypointOutput(t, ctx, "boots"))
}

const zeros = "0000000000000000000000000000000000000000000000000000000000000000"

func createRequest(id, image string) *v1alpha2.CreateMachineRequest {
	return &v1alpha2.CreateMachineRequest{
		MachineId:     id,
		RequestId:     "create-" + id,
		ImageDigest:   image,
		Resources:     &v1alpha2.Resources{CpuCount: 1, MemoryBytes: 256 << 20},
		RootDiskBytes: 1 << 30,
	}
}

// The converter applies the OCI layer specification's rules: each fixture is an image of layers
// pulled through the real converter, and the tree its EROFS holds is the specification's result.
func TestE2E_ConversionAppliesTheLayerSpecification(t *testing.T) {
	// The lower layer of the same-layer fixtures: a file and a directory with a child.
	lower := layer(file("f", "old"), dir("d"), file("d/old", "O"))
	fixtures := []struct {
		name   string
		layers [][]byte
		want   []string
	}{
		{
			"a whiteout hides what a lower layer holds",
			[][]byte{
				layer(dir("d"), file("d/a", "A"), file("d/b", "B"), file("x", "X"), file("keep", "K")),
				layer(whiteout("x"), whiteout("d/a")),
			},
			[]string{"d/", "d/b=B", "keep=K"},
		},
		{
			"an opaque whiteout hides every lower child of its directory and not the layer's own",
			[][]byte{
				layer(dir("d"), file("d/a", "A"), file("d/b", "B"), dir("e"), file("e/f", "F")),
				layer(dir("d"), opaque("d"), file("d/c", "C")),
			},
			[]string{"d/", "d/c=C", "e/", "e/f=F"},
		},
		{
			"a directory a later layer lists without the opaque marker keeps its lower children",
			[][]byte{
				layer(dir("d"), file("d/a", "A")),
				layer(dir("d"), file("d/b", "B")),
			},
			[]string{"d/", "d/a=A", "d/b=B"},
		},
		{
			"a directory replaced by a file loses its children",
			[][]byte{
				layer(dir("d"), file("d/a", "A"), file("d/b", "B"), file("keep", "K")),
				layer(file("d", "now a file")),
			},
			[]string{"d=now a file", "keep=K"},
		},
		{
			"a file replaced by a directory takes the directory",
			[][]byte{
				layer(file("f", "was a file"), file("keep", "K")),
				layer(dir("f"), file("f/g", "G")),
			},
			[]string{"f/", "f/g=G", "keep=K"},
		},
		{
			"a whiteout before a file of its name in one layer leaves the layer's file",
			[][]byte{lower, layer(whiteout("f"), file("f", "new"))},
			[]string{"f=new", "d/", "d/old=O"},
		},
		{
			"a whiteout after a file of its name in one layer leaves the layer's file",
			[][]byte{lower, layer(file("f", "new"), whiteout("f"))},
			[]string{"f=new", "d/", "d/old=O"},
		},
		{
			"a whiteout before a directory of its name in one layer leaves only the layer's children",
			[][]byte{lower, layer(whiteout("d"), dir("d"), file("d/new", "N"))},
			[]string{"f=old", "d/", "d/new=N"},
		},
		{
			"a whiteout after a directory of its name in one layer leaves only the layer's children",
			[][]byte{lower, layer(dir("d"), file("d/new", "N"), whiteout("d"))},
			[]string{"f=old", "d/", "d/new=N"},
		},
		{
			"a whiteout of a name the layer holds only children of leaves the layer's children",
			[][]byte{lower, layer(file("d/new", "N"), whiteout("d"))},
			[]string{"f=old", "d/", "d/new=N"},
		},
	}

	n := startNode(t)
	for i, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			pulled := n.pull(t, "fixture-"+string(rune('a'+i)), fixture.layers...)
			assert.ElementsMatch(t, fixture.want, tree(t, n.erofsOf(t, pulled.Digest)))
		})
	}
}

// A file of the cache that is not what the pull recorded fails the check before boot: the node
// measures fs-verity on the file and compares it with the digest recorded at the pull.
func TestE2E_ATamperedImageFailsTheCheckBeforeBoot(t *testing.T) {
	n := startNode(t)
	n.arm(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	variants := []struct {
		name   string
		tamper func(t *testing.T, erofs string)
	}{
		// A copy with the same bytes, without fs-verity: the file is no longer sealed.
		{"a replacement without fs-verity", func(t *testing.T, erofs string) { replace(t, erofs, readFile(t, erofs), false) }},
		// Other bytes, sealed: the file is a verity file whose digest is not the recorded one.
		{"a replacement with other bytes under fs-verity", func(t *testing.T, erofs string) {
			content := readFile(t, erofs)
			content[len(content)/2] ^= 0xff
			replace(t, erofs, content, true)
		}},
	}
	for i, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			pulled := n.pullBootable(t, "tamper-"+string(rune('a'+i)), layer(file("tag", variant.name)))
			erofs := n.erofsOf(t, pulled.Digest)

			// The control: the untampered image passes the check that precedes a boot, and boots.
			control := "control-" + string(rune('a'+i))
			_, err := n.client.CreateMachine(ctx, createRequest(control, pulled.Digest))
			require.NoError(t, err, "the untampered image passes the check")
			_, err = n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: control})
			require.NoError(t, err, "the untampered image boots")
			assert.Equal(t, "hello from the entrypoint\n", n.entrypointOutput(t, ctx, control))
			_, err = n.client.CreateMachine(ctx, createRequest("victim-"+string(rune('a'+i)), pulled.Digest))
			require.NoError(t, err)

			variant.tamper(t, erofs)

			_, err = n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "victim-" + string(rune('a'+i))})
			require.Error(t, err, "a tampered image does not boot")
			assert.Equal(t, failed, v1alpha2.KindOf(err))
			assert.ErrorContains(t, err, "fs-verity", "the failure names the check that refused the file")

			_, err = n.client.CreateMachine(ctx, createRequest("late-"+string(rune('a'+i)), pulled.Digest))
			require.Error(t, err, "nor does a Machine get created from it")
			assert.ErrorContains(t, err, "fs-verity")
		})
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return b
}

// replace puts a file with the content where the file was. A file under fs-verity cannot be
// written, so the new one is made beside it and renamed over it, as an attacker with the node's
// disk would.
func replace(t *testing.T, path string, content []byte, sealed bool) {
	t.Helper()
	next := filepath.Join(filepath.Dir(path), "tampered")
	require.NoError(t, os.WriteFile(next, content, 0o444))
	if sealed {
		seal(t, next)
	}
	require.NoError(t, os.Rename(next, path))
}

// seal enables fs-verity on the file, SHA-256 over blocks of 4096 bytes.
func seal(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	arg := unix.FsverityEnableArg{Version: 1, Hash_algorithm: unix.FS_VERITY_HASH_ALG_SHA256, Block_size: 4096}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.FS_IOC_ENABLE_VERITY, uintptr(unsafe.Pointer(&arg)))
	require.Zero(t, errno, "enable fs-verity on the replacement")
}
