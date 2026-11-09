package imagestore

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// verityBlockSize is the block size of the Merkle tree (ADR 0008).
const verityBlockSize = 4096

// errVerityUnsupported is what enabling or measuring fs-verity returns on a
// file system that does not provide it.
var errVerityUnsupported = errors.New("fs-verity is not supported by the file system")

// enableVerity makes the file a verity file: SHA-256 over 4096-byte blocks.
// The kernel refuses while any descriptor of the file is open for writing.
func enableVerity(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	arg := unix.FsverityEnableArg{
		Version:        1,
		Hash_algorithm: unix.FS_VERITY_HASH_ALG_SHA256,
		Block_size:     verityBlockSize,
	}
	if err := ioctl(f, unix.FS_IOC_ENABLE_VERITY, unsafe.Pointer(&arg)); err != nil {
		return verityError("enable fs-verity", err)
	}
	return nil
}

// measureVerity returns the fs-verity digest of the file as "sha256:<hex>", or
// an error naming that the file is not a verity file.
func measureVerity(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	// The kernel's fsverity_digest header is followed by the digest bytes.
	buf := make([]byte, unsafe.Sizeof(unix.FsverityDigest{})+64)
	header := (*unix.FsverityDigest)(unsafe.Pointer(&buf[0]))
	header.Size = 64
	if err := ioctl(f, unix.FS_IOC_MEASURE_VERITY, unsafe.Pointer(&buf[0])); err != nil {
		if errors.Is(err, unix.ENODATA) {
			return "", errors.New("the file is not a verity file")
		}
		return "", verityError("measure fs-verity", err)
	}
	if header.Algorithm != unix.FS_VERITY_HASH_ALG_SHA256 || header.Size != 32 {
		return "", fmt.Errorf("the file's fs-verity digest is algorithm %d of %d bytes, not SHA-256", header.Algorithm, header.Size)
	}
	offset := unsafe.Sizeof(unix.FsverityDigest{})
	return "sha256:" + hex.EncodeToString(buf[offset:offset+32]), nil
}

func ioctl(f *os.File, request uintptr, arg unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), request, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

func verityError(step string, err error) error {
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTTY) {
		return fmt.Errorf("%s: %w", step, errVerityUnsupported)
	}
	return fmt.Errorf("%s: %w", step, err)
}
