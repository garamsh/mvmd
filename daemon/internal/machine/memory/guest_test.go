package memory_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// searchedFiles returns what SearchFiles of the memory Guest sends for a
// Machine holding the files.
func searchedFiles(t *testing.T, files []string, dir, pattern string) ([]string, error) {
	t.Helper()
	g := memory.NewGuest(0)
	for _, f := range files {
		g.PutFile("m", f, "")
		_, held := g.File("m", f)
		require.True(t, held, "the setup holds %s", f)
	}
	var got []string
	err := g.SearchFiles(context.Background(), "m", dir, pattern, func(run []string) error {
		got = append(got, run...)
		return nil
	})
	return got, err
}

// Each case pins what the Guest Agent's glob does, as globset 0.4.21 with a
// literal separator matches it: the paths are relative to /d/, "found" must be
// sent and "missed" must not, and a pattern marked "!" must be refused. The cases
// are in testdata/glob_cases.tsv, of which the Guest Agent's own tests read a
// copy that `make test` holds to be the same file, so the two cannot disagree.
func TestSearchFiles_MatchesTheAgentsGlob(t *testing.T) {
	raw, err := os.ReadFile("testdata/glob_cases.tsv")
	require.NoError(t, err)
	paths := func(column string) []string {
		var out []string
		for p := range strings.SplitSeq(column, "|") {
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	checked := 0
	for line := range strings.SplitSeq(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		columns := strings.Split(line, "\t")
		require.Len(t, columns, 4, "a case has four columns: %q", line)
		checked++
		t.Run(columns[0], func(t *testing.T) {
			pattern := columns[1]
			if columns[2] == "!" {
				_, err := searchedFiles(t, []string{"/d/a"}, "/d/", pattern)
				assert.Error(t, err, "the pattern %q is refused", pattern)
				return
			}
			var files, want []string
			for _, f := range paths(columns[2]) {
				files = append(files, "/d/"+f)
				want = append(want, "/d/"+f)
			}
			for _, f := range paths(columns[3]) {
				files = append(files, "/d/"+f)
			}
			got, err := searchedFiles(t, files, "/d/", pattern)
			require.NoError(t, err)
			assert.ElementsMatch(t, want, got)
		})
	}
	require.GreaterOrEqual(t, checked, 50, "the cases were read")
}

func TestSearchFiles_SendsSortedByPath(t *testing.T) {
	got, err := searchedFiles(t, []string{"/d/b/c", "/d/a", "/d/b/a", "/d/c"}, "/d/", "**")
	require.NoError(t, err)
	assert.Equal(t, []string{"/d/a", "/d/b/a", "/d/b/c", "/d/c"}, got)
}

func TestSearchFiles_MatchesRelativeToTheDirectory(t *testing.T) {
	files := []string{"/d/a.go", "/d/e/b.go", "/dd/c.go", "/e/d.go", "/d.go"}

	got, err := searchedFiles(t, files, "/d/", "*.go")
	require.NoError(t, err)
	assert.Equal(t, []string{"/d/a.go"}, got, "the files directly under the directory, and none outside it")

	got, err = searchedFiles(t, files, "/d", "*.go")
	require.NoError(t, err)
	assert.Equal(t, []string{"/d/a.go"}, got, "a directory path without its trailing slash is the same directory, not a prefix of /dd")

	got, err = searchedFiles(t, files, "/", "**/*.go")
	require.NoError(t, err)
	assert.Equal(t, []string{"/d.go", "/d/a.go", "/d/e/b.go", "/dd/c.go", "/e/d.go"}, got, "control: the root reaches them all")
}

func TestSearchFiles_RefusesAPatternTheAgentRefuses(t *testing.T) {
	for _, pattern := range []string{"[ab", "[c-a]", "a}", "{a", `a\`, `\{a,b}`} {
		t.Run(pattern, func(t *testing.T) {
			_, err := searchedFiles(t, []string{"/d/a"}, "/d/", pattern)
			require.ErrorIs(t, err, machine.ErrPatternInvalid)
			assert.Contains(t, err.Error(), pattern)
		})
	}
	got, err := searchedFiles(t, []string{"/d/{a"}, "/d/", "[{]a")
	require.NoError(t, err)
	assert.Equal(t, []string{"/d/{a"}, got, "control: the way the agent's message says to write a brace")
}
