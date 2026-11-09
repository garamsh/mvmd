package ociregistry

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"

	"github.com/garamsh/mvmd/daemon/internal/image"
)

var (
	// errSize is the failure of a blob whose length is not its descriptor's.
	errSize = errors.New("blob length differs from its descriptor")
	// errDigest is the failure of a blob whose SHA-256 is not its descriptor's.
	errDigest = errors.New("blob does not hash to its descriptor")
)

// verifying reads a blob and fails at the first byte past the descriptor's
// size, or at the end when the length or the SHA-256 differs. It never hands
// on a byte past the size.
type verifying struct {
	body io.ReadCloser
	want image.Descriptor
	sum  hash.Hash
	read int64
	err  error
}

func newVerifying(body io.ReadCloser, want image.Descriptor) *verifying {
	return &verifying{body: body, want: want, sum: sha256.New()}
}

func (v *verifying) Read(p []byte) (int, error) {
	if v.err != nil {
		return 0, v.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	remaining := v.want.Size - v.read
	// One byte more than remains is asked for, so an excess is seen and not
	// returned.
	if int64(len(p)) > remaining+1 {
		p = p[:remaining+1]
	}
	n, err := v.body.Read(p)
	if int64(n) > remaining {
		n = int(remaining)
		err = fmt.Errorf("blob %s is longer than its size %d: %w", v.want.Digest, v.want.Size, errSize)
	}
	v.sum.Write(p[:n])
	v.read += int64(n)
	if errors.Is(err, io.EOF) {
		switch {
		case v.read != v.want.Size:
			err = fmt.Errorf("blob %s is %d bytes, not %d: %w", v.want.Digest, v.read, v.want.Size, errSize)
		case "sha256:"+hex.EncodeToString(v.sum.Sum(nil)) != v.want.Digest:
			err = fmt.Errorf("blob %s: %w", v.want.Digest, errDigest)
		}
	}
	if err != nil {
		v.err = err
	}
	return n, err
}

func (v *verifying) Close() error { return v.body.Close() }

// readAll reads a blob of at most limit bytes to its end and returns its bytes
// only if they are the descriptor's.
func readAll(body io.ReadCloser, want image.Descriptor, limit int64) ([]byte, error) {
	defer body.Close()
	if want.Size < 0 || want.Size > limit {
		return nil, fmt.Errorf("blob %s declares %d bytes, outside 0 to %d: %w", want.Digest, want.Size, limit, errSize)
	}
	return io.ReadAll(newVerifying(body, want))
}
