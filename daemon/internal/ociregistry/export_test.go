package ociregistry

// The failures are unexported, since nothing outside the package branches on
// them; the external tests tell them apart through these.
var (
	ErrSize   = errSize
	ErrDigest = errDigest
)
