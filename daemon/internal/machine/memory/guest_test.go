package memory_test

import (
	"context"
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
// sent and "missed" must not. A case with no "missed" is a pattern that
// matches every path there is.
func TestSearchFiles_MatchesTheAgentsGlob(t *testing.T) {
	tests := []struct {
		name, pattern string
		found, missed []string
	}{
		{"a literal is the whole path, not a substring", "a", []string{"a"}, []string{"x/a/y", "a/y", "ax", "xa"}},
		{"star stays within a segment", "*.go", []string{"b.go", ".go"}, []string{"a/b.go", "b.gox", "b.txt"}},
		{"star matches nothing at all", "a*b", []string{"ab", "axxb"}, []string{"a/b", "a/x/b"}},
		{"question mark is one character of a segment", "a?b", []string{"axb"}, []string{"a/b", "ab", "axxb"}},
		{"question mark is one byte, so not a two-byte character", "?.go", []string{"a.go"}, []string{"é.go", "ab.go"}},
		{"two question marks match a two-byte character", "??.go", []string{"é.go"}, []string{"a.go"}},
		{"star matches a multi-byte character", "*.go", []string{"é.go"}, []string{"é.gox"}},

		{"double star at the start crosses segments", "**/*.go", []string{"b.go", "a/b.go", "a/c/b.go"}, []string{"b.txt", "a/b.txt"}},
		{"double star at the start matches a whole segment name", "**/b", []string{"b", "a/b"}, []string{"a/xb", "ab"}},
		{"double star in the middle crosses zero or more segments", "a/**/b", []string{"a/b", "a/x/b", "a/x/y/b"}, []string{"a/xb", "ab", "x/a/b"}},
		{"double star at the end matches what is below", "a/**", []string{"a/b", "a/b/c"}, []string{"ab/c", "b/a/c"}},
		{"double star alone matches every path", "**", []string{"a", "a/b/c"}, nil},
		{"double star and a separator alone match every path", "**/", []string{"a", "a/b/c"}, nil},
		{"double star that ends a name is two stars", "a**", []string{"a", "ab"}, []string{"a/b"}},
		{"double star inside a name is two stars", "a**b", []string{"ab", "axxb"}, []string{"a/b", "a/x/b"}},
		{"double star that starts a name is two stars", "**a", []string{"xa"}, []string{"x/a"}},
		{"double star after a separator and before a name is two stars", "a/**b", []string{"a/xb"}, []string{"a/x/b"}},

		{"alternates choose between literals", "{a,b}.go", []string{"a.go", "b.go"}, []string{"c.go", "ab.go", "a/b.go"}},
		{"alternates nest", "{a,{b,c}}", []string{"a", "b", "c"}, []string{"d", "bc"}},
		{"an alternate may hold a separator", "{a,b/c}.go", []string{"a.go", "b/c.go"}, []string{"b.go", "c.go"}},
		{"an alternate may hold a star, which stays within a segment", "{a,*.x}", []string{"a", "z.x"}, []string{"a/z.x"}},
		{"an alternate may start with a double star", "{**/a,b}", []string{"a", "x/a", "b"}, []string{"x/b"}},
		{"an alternate may end with a double star", "{a/**,b}", []string{"a/x", "b"}, []string{"b/x", "a"}},
		{"a double star after an alternate opens crosses segments", "x/{**/a,b}", []string{"x/a", "x/y/a", "x/b"}, []string{"x/y/b"}},
		{"an empty alternate at the end is dropped", "{a,}x", []string{"ax"}, []string{"x"}},
		{"an empty alternate at the start is dropped", "{,a}x", []string{"ax"}, []string{"x"}},
		{"a nested empty alternate is dropped", "{a,{b,}}x", []string{"ax", "bx"}, []string{"x"}},
		{"a group of no alternate is no pattern at all", "{}x", []string{"x"}, []string{"ax"}},
		{"an escaped comma is a literal in an alternate", `{a\,b,c}`, []string{"a,b", "c"}, []string{"a", "b"}},
		{"a comma outside alternates is a literal", "a,b", []string{"a,b"}, []string{"a", "xa,by"}},
		{"a closing brace in a class is a literal", "[}]", []string{"}"}, []string{"a"}},

		{"a class matches one of its characters", "[ab].go", []string{"a.go", "b.go"}, []string{"c.go", "ab.go"}},
		{"a class holds a range", "[a-c]", []string{"a", "b", "c"}, []string{"d"}},
		{"a class negates with an exclamation mark", "[!a]", []string{"b"}, []string{"a", "ab"}},
		{"a class negates with a caret", "[^a]", []string{"b"}, []string{"a"}},
		{"a negated class matches a separator", "a[!a]b", []string{"acb", "a/b"}, []string{"aab"}},
		{"a negated class is one byte, so not a two-byte character", "[!a]", []string{"b"}, []string{"é", "a"}},
		{"a class may hold a separator", "a[/]b", []string{"a/b"}, []string{"ab"}},
		{"a first closing bracket is a literal", "[]a]", []string{"]", "a"}, []string{"b"}},
		{"a first closing bracket after a negation is a literal", "[!]a]", []string{"b"}, []string{"]", "a"}},
		{"a trailing dash is a literal", "[a-]", []string{"-", "a"}, []string{"b"}},
		{"a leading dash is a literal", "[-a]", []string{"-", "a"}, []string{"b"}},
		{"a dash after a range extends the range", "[a-c-e]", []string{"a", "d", "e"}, []string{"-", "f"}},
		{"a class holds a two-byte character as bytes", "[é]", nil, []string{"é"}},
		{"a closing bracket outside a class is a literal", "a]", []string{"a]"}, []string{"a", "xa]y"}},

		{"a backslash makes a star a literal", `\*.go`, []string{"*.go"}, []string{"a.go"}},
		{"a backslash makes a question mark a literal", `\?`, []string{"?"}, []string{"a"}},
		{"a backslash makes a bracket a literal", `\[a]`, []string{"[a]"}, []string{"a"}},
		{"a backslash before an ordinary character is that character", `\a`, []string{"a"}, []string{`\a`}},
		{"a double backslash is one backslash", `\\`, []string{`\`}, []string{`\\`}},
		{"an escaped separator is a separator", `a\/b`, []string{"a/b"}, []string{"ab"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var files, want []string
			for _, f := range tt.found {
				files = append(files, "/d/"+f)
				want = append(want, "/d/"+f)
			}
			for _, f := range tt.missed {
				files = append(files, "/d/"+f)
			}
			got, err := searchedFiles(t, files, "/d/", tt.pattern)
			require.NoError(t, err)
			assert.ElementsMatch(t, want, got)
		})
	}
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
