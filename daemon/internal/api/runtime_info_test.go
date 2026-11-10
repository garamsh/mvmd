package api_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/api"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// driverCapabilities returns every constant of type machine.Capability that a
// non-test file of the machine package declares, read from the source so that
// a capability added there, in any file, is covered without this test naming it.
func driverCapabilities(t *testing.T) []machine.Capability {
	t.Helper()
	const dir = "../machine"
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var caps []machine.Capability
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Capability" {
					continue
				}
				require.Len(t, vs.Values, 1)
				lit, ok := vs.Values[0].(*ast.BasicLit)
				require.True(t, ok, "%s is a string literal", vs.Names[0].Name)
				value, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				caps = append(caps, machine.Capability(value))
			}
		}
	}
	require.NotEmpty(t, caps, "the capabilities were read back from the machine package")
	return caps
}

func runtimeInfo(t *testing.T, caps ...machine.Capability) []v1alpha2.Capability {
	t.Helper()
	srv := api.New(nil, nil, nil, []machine.DriverInfo{{Name: "d", Version: "1", Capabilities: caps}}, nil)
	resp, err := srv.GetRuntimeInfo(context.Background(), &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetDrivers(), 1)
	return resp.GetDrivers()[0].GetCapabilities()
}

func TestGetRuntimeInfo_EveryDriverCapabilityHasAWireValue(t *testing.T) {
	for _, c := range driverCapabilities(t) {
		t.Run(string(c), func(t *testing.T) {
			got := runtimeInfo(t, c)
			assert.NotEmpty(t, got, "the capability is reported, not dropped")
			assert.NotContains(t, got, v1alpha2.Capability_CAPABILITY_UNSPECIFIED)
		})
	}
}

func TestGetRuntimeInfo_ReportsSnapshotAndConsole(t *testing.T) {
	assert.Equal(t, []v1alpha2.Capability{v1alpha2.Capability_CAPABILITY_SNAPSHOT},
		runtimeInfo(t, machine.CapabilitySnapshot))
	assert.Equal(t, []v1alpha2.Capability{v1alpha2.Capability_CAPABILITY_CONSOLE},
		runtimeInfo(t, machine.CapabilityConsole))
	assert.Empty(t, runtimeInfo(t), "a Driver with no capability reports none")
}
