package vmmconfig_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/vmmconfig"
)

type dirs struct{ dir string }

func (d dirs) RunDir(machine.MachineID) (string, error) { return d.dir, nil }

type failingDirs struct{}

func (failingDirs) RunDir(machine.MachineID) (string, error) {
	return "", errors.New("no such Machine")
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// node is files in a temporary directory with the digests they have.
func node(t *testing.T) vmmconfig.Files {
	t.Helper()
	root := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(root, name)
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
		return p
	}
	return vmmconfig.Files{
		Binary: write("vmm", "a binary"), BinarySHA256: sum([]byte("a binary")),
		Kernels: []machine.KernelFile{
			{Path: write("vmlinux", "a kernel"), SHA256: sum([]byte("a kernel"))},
			{Path: write("vmlinux-next", "another kernel"), SHA256: sum([]byte("another kernel"))},
		},
		Initrd: write("initrd.cpio", "an initrd"), InitrdSHA256: sum([]byte("an initrd")),
	}
}

func TestValidate_RefusesFilesThatNameNoAbsolutePathOrNoDigestAndNamesTheVMM(t *testing.T) {
	good := node(t)
	require.NoError(t, vmmconfig.Validate("a-vmm", good), "control: the node's own files are accepted")

	zero := strings.Repeat("0", 64)
	tests := []struct {
		name   string
		change func(*vmmconfig.Files)
		want   string
	}{
		{"a relative binary path", func(f *vmmconfig.Files) { f.Binary = "vmm" }, `a-vmm binary "vmm" is not an absolute path`},
		{"a relative kernel path", func(f *vmmconfig.Files) {
			f.Kernels = []machine.KernelFile{{Path: "vmlinux", SHA256: zero}}
		}, `a-vmm kernel "vmlinux" is not an absolute path`},
		{"a relative path on a later kernel", func(f *vmmconfig.Files) {
			f.Kernels = []machine.KernelFile{f.Kernels[0], {Path: "vmlinux", SHA256: zero}}
		}, `a-vmm kernel "vmlinux" is not an absolute path`},
		{"no kernel", func(f *vmmconfig.Files) { f.Kernels = nil }, "a-vmm lists no kernel"},
		{"a relative initrd path", func(f *vmmconfig.Files) { f.Initrd = "initrd" }, `a-vmm initrd "initrd" is not an absolute path`},
		{"a binary digest that is not hex", func(f *vmmconfig.Files) { f.BinarySHA256 = "sha256:abc" }, `a-vmm binary digest "sha256:abc" is not 64 lowercase hex digits`},
		{"an upper-case kernel digest", func(f *vmmconfig.Files) {
			f.Kernels = []machine.KernelFile{{Path: f.Kernels[0].Path, SHA256: strings.ToUpper(sum([]byte("a kernel")))}}
		}, "a-vmm kernel digest"},
		{"an initrd digest that is not hex", func(f *vmmconfig.Files) { f.InitrdSHA256 = "sha256:abc" }, "a-vmm initrd digest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := good
			tt.change(&f)
			err := vmmconfig.Validate("a-vmm", f)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestCheck_RefusesANodeThatCannotStartAVMMAndNamesTheVMM(t *testing.T) {
	good := node(t)
	d := dirs{t.TempDir()}
	require.NoError(t, vmmconfig.Check("a-vmm", good, d), "control: the node's own files are accepted")

	zero := strings.Repeat("0", 64)
	tests := []struct {
		name   string
		change func(*vmmconfig.Files)
		want   string
	}{
		{"a binary that is not the pinned one", func(f *vmmconfig.Files) { f.BinarySHA256 = zero }, "the a-vmm binary"},
		{"a kernel that is not the pinned one", func(f *vmmconfig.Files) {
			f.Kernels = []machine.KernelFile{{Path: f.Kernels[0].Path, SHA256: zero}, f.Kernels[1]}
		}, "the a-vmm kernel"},
		{"a later kernel that is not the pinned one", func(f *vmmconfig.Files) {
			f.Kernels = []machine.KernelFile{f.Kernels[0], {Path: f.Kernels[1].Path, SHA256: zero}}
		}, "the a-vmm kernel"},
		{"an initrd that is not the pinned one", func(f *vmmconfig.Files) { f.InitrdSHA256 = zero }, "the a-vmm initrd"},
		{"a missing initrd", func(f *vmmconfig.Files) { f.Initrd = "/nonexistent/initrd" }, "the a-vmm initrd"},
		{"files Validate refuses", func(f *vmmconfig.Files) { f.Initrd = "initrd" }, "is not an absolute path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := good
			tt.change(&f)
			err := vmmconfig.Check("a-vmm", f, d)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}

	err := vmmconfig.Check("a-vmm", good, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a-vmm needs the run directories of Machines")
}

func TestRunPathAndInRun_NameAFileOfTheRunDirectoryOnTheHostAndInTheVMMsRoot(t *testing.T) {
	got, err := vmmconfig.RunPath(dirs{"/run/m1"}, "m1", vmmconfig.APISocket)
	require.NoError(t, err)
	assert.Equal(t, "/run/m1/api.sock", got)

	_, err = vmmconfig.RunPath(failingDirs{}, "m1", vmmconfig.APISocket)
	assert.ErrorContains(t, err, "no such Machine")

	assert.Equal(t, filepath.Join(machine.InRunDir, "vsock.sock"), vmmconfig.InRun(vmmconfig.VsockSocket))
}
