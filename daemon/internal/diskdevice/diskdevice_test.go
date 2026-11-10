package diskdevice_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/diskdevice"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func TestName_TheImageIsNamedByItsPosition(t *testing.T) {
	assert.Equal(t, "disk0", diskdevice.Name(0, ""))
	assert.Equal(t, "disk3", diskdevice.Name(3, ""))
}

func TestName_ADiskIsNamedByTheHexOfItsIDsSHA256AtAnyPosition(t *testing.T) {
	sum := sha256.Sum256([]byte("data"))
	want := "disk_" + hex.EncodeToString(sum[:])

	assert.Equal(t, want, diskdevice.Name(0, "data"))
	assert.Equal(t, want, diskdevice.Name(5, "data"), "the position does not name a disk")
	assert.NotEqual(t, want, diskdevice.Name(0, "other"), "control: another ID names another device")
}

func TestName_IsWithinAFileNameAndNeverStartsWithTheReservedPrefix(t *testing.T) {
	long := machine.DiskID(strings.Repeat("x", 4096))
	ids := []machine.DiskID{"", "a", "__reserved", long, "with/slash and space"}
	for _, id := range ids {
		name := diskdevice.Name(0, id)
		assert.LessOrEqual(t, len(name), 255)
		assert.False(t, strings.HasPrefix(name, "__"), name)
	}
	assert.Equal(t, len(diskdevice.Name(0, "a")), len(diskdevice.Name(0, long)), "the length does not follow the ID's")
}

func TestRemoveLink_RemovesALinkThatIsPresentAndAcceptsOneThatIsNot(t *testing.T) {
	link := filepath.Join(t.TempDir(), "disk_x")
	require.NoError(t, os.WriteFile(link, []byte("disk"), 0o600))

	require.NoError(t, diskdevice.RemoveLink(link))
	_, err := os.Lstat(link)
	assert.ErrorIs(t, err, os.ErrNotExist, "control: the link that was present is gone")

	assert.NoError(t, diskdevice.RemoveLink(link), "an absent link succeeds")
}
