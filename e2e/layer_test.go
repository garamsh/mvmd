//go:build e2e_real

package e2e_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"path"
	"runtime"
	"time"
)

// entry is one entry of a layer, written as the OCI image specification writes a layer's changes.
type entry struct {
	name     string
	typeflag byte
	mode     int64
	data     string
}

func dir(name string) entry { return entry{name: name, typeflag: tar.TypeDir, mode: 0o755} }
func file(name, data string) entry {
	return entry{name: name, typeflag: tar.TypeReg, mode: 0o644, data: data}
}

// executable is a regular file the guest can run.
func executable(name, data string) entry {
	return entry{name: name, typeflag: tar.TypeReg, mode: 0o755, data: data}
}

// whiteout is the entry that deletes the named path of the layers below.
func whiteout(name string) entry { return file(path.Join(path.Dir(name), ".wh."+path.Base(name)), "") }

// opaque is the entry that hides every child of the directory in the layers below.
func opaque(directory string) entry { return file(path.Join(directory, ".wh..wh..opq"), "") }

func goArch() string { return runtime.GOARCH }

// layer is the uncompressed tar archive of the entries, in that order.
func layer(entries ...entry) []byte {
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	for _, e := range entries {
		header := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     e.mode,
			Size:     int64(len(e.data)),
			ModTime:  time.Unix(1_700_000_000, 0),
			Format:   tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(header); err != nil {
			panic("a buffer takes the header of " + e.name + ": " + err.Error())
		}
		if _, err := tw.Write([]byte(e.data)); err != nil {
			panic("a buffer takes the data of " + e.name + ": " + err.Error())
		}
	}
	if err := tw.Close(); err != nil {
		panic("a buffer takes the end of the archive: " + err.Error())
	}
	return out.Bytes()
}

// gzipped compresses a layer the way registries serve one.
func gzipped(raw []byte) []byte {
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(raw); err != nil {
		panic("a buffer takes the bytes: " + err.Error())
	}
	if err := zw.Close(); err != nil {
		panic("a buffer takes the end of the stream: " + err.Error())
	}
	return out.Bytes()
}
