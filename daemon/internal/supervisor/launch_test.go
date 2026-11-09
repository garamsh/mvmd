package supervisor

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/garamsh/mvmd/daemon/internal/supervisorapi/v1alpha1"
)

// Only a VMM that opens its tap by name shares the host's network namespace:
// one with no network device, as a converter, and one that takes its tap by
// descriptor each get a namespace of their own (ADR 0015).
func TestNewNetworkNamespace_OnlyAVMMThatOpensItsTapByNameSharesTheHosts(t *testing.T) {
	for _, c := range []struct {
		name         string
		tap          string
		byDescriptor bool
		want         bool
	}{
		{"no network device", "", false, true},
		{"a tap by descriptor", "tap0", true, true},
		{"a tap by name", "tap0", false, false},
	} {
		req := &pb.StartRequest{Tap: c.tap, Process: &pb.Process{TapByDescriptor: c.byDescriptor}}
		assert.Equal(t, c.want, newNetworkNamespace(req), c.name)
	}
}

func validRequest() *pb.StartRequest {
	return &pb.StartRequest{
		Process: &pb.Process{
			Executable:       "/usr/libexec/vmm",
			ExecutableSha256: strings.Repeat("a", 64),
			BootFiles: []*pb.BlockDevice{
				{Path: "/var/kernel", InPath: "/boot/kernel", ReadOnly: true, Sha256: strings.Repeat("b", 64)},
				{Path: "/var/m1/root.img", InPath: "/disk/0"},
				{Path: "/var/m1/data.img", InPath: "/disk/1"},
			},
			ReadFiles: []string{"/sys/devices/system/cpu/cpu0/cache"},
		},
		RunDir: "/var/m1/run", RunDirInPath: "/run", Cgroup: "/sys/fs/cgroup/m1",
	}
}

func TestCheckRequest_RefusesAnInRootPathThatCollidesOrLeavesTheRoot(t *testing.T) {
	require.NoError(t, checkRequest(validRequest()), "control: the request without a fault is accepted")

	for _, c := range []struct {
		name   string
		change func(*pb.StartRequest)
		want   string
	}{
		{"a boot file with no path in the root, as an earlier mvmd sent it", func(r *pb.StartRequest) { r.Process.BootFiles[1].InPath = "" }, "absolute, clean"},
		{"no path for the run directory in the root", func(r *pb.StartRequest) { r.RunDirInPath = "" }, "absolute, clean"},
		{"a relative path in the root", func(r *pb.StartRequest) { r.Process.BootFiles[1].InPath = "disk/0" }, "absolute, clean"},
		{"a path that climbs out of the root", func(r *pb.StartRequest) { r.Process.BootFiles[1].InPath = "/disk/../../etc/passwd" }, "absolute, clean"},
		{"the root itself", func(r *pb.StartRequest) { r.RunDirInPath = "/" }, "cannot be the path"},
		{"a path under /dev, which the root's own /dev replaces", func(r *pb.StartRequest) { r.Process.BootFiles[1].InPath = "/dev/disk0" }, "cannot be the path"},
		{"two files at one path", func(r *pb.StartRequest) { r.Process.BootFiles[2].InPath = "/disk/0" }, "both given the path /disk/0"},
		{"a file at the run directory's path", func(r *pb.StartRequest) { r.Process.BootFiles[2].InPath = "/run" }, "both given the path /run"},
		{"a path inside another", func(r *pb.StartRequest) { r.Process.BootFiles[2].InPath = "/disk/0/inner" }, "is inside"},
		{"a digest that is not hex", func(r *pb.StartRequest) { r.Process.BootFiles[0].Sha256 = "sha256:abc" }, "64 lowercase hex"},
		{"an executable digest that is not hex", func(r *pb.StartRequest) { r.Process.ExecutableSha256 = strings.Repeat("A", 64) }, "64 lowercase hex"},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := validRequest()
			c.change(req)
			err := checkRequest(req)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.want)
		})
	}
}

func TestMountsFor_PutsEachFileAtItsPathInTheRoot(t *testing.T) {
	mounts, err := mountsFor(validRequest())
	require.NoError(t, err)

	got := map[string]bindMount{}
	for _, m := range mounts {
		got[m.path] = m
	}
	assert.Equal(t, bindMount{host: "/var/kernel", path: "/boot/kernel", readOnly: true, sha256: strings.Repeat("b", 64)}, got["/boot/kernel"])
	assert.Equal(t, bindMount{host: "/var/m1/root.img", path: "/disk/0"}, got["/disk/0"])
	assert.Equal(t, bindMount{host: "/var/m1/run", path: "/run"}, got["/run"], "the run directory is read-write")
	assert.Equal(t, bindMount{host: "/usr/libexec/vmm", path: "/usr/libexec/vmm", readOnly: true, executable: true, sha256: strings.Repeat("a", 64)}, got["/usr/libexec/vmm"],
		"the executable keeps its host path and its pin")
	assert.Equal(t, bindMount{host: "/sys/devices/system/cpu/cpu0/cache", path: "/sys/devices/system/cpu/cpu0/cache", readOnly: true}, got["/sys/devices/system/cpu/cpu0/cache"])
	for _, host := range []string{"/var/kernel", "/var/m1/root.img", "/var/m1/run"} {
		assert.NotContains(t, got, host, "a host path is not a path in the root")
	}
	assert.Len(t, mounts, 6)
}

func TestMountsFor_AFileGivenTwiceIsMountedOnce(t *testing.T) {
	req := validRequest()
	req.Process.ReadFiles = append(req.Process.ReadFiles, req.Process.Executable)
	mounts, err := mountsFor(req)
	require.NoError(t, err)
	n := 0
	for _, m := range mounts {
		if m.path == "/usr/libexec/vmm" {
			n++
			assert.True(t, m.executable, "the executable stays executable")
			assert.Equal(t, strings.Repeat("a", 64), m.sha256, "and keeps its pin")
		}
	}
	assert.Equal(t, 1, n)
}
