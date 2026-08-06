package actions

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tarball builds a gzipped tar from the given entries, in order.
func tarball(t *testing.T, entries []*tar.Header, bodies []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i, h := range entries {
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(bodies[i]))
		}
		require.NoError(t, tw.WriteHeader(h))
		if h.Typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(bodies[i]))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// A symlink to an absolute path followed by a file written through it is an
// arbitrary write on the runner HOST, outside the job sandbox, reachable by any
// workflow that names the action. The entry names alone pass a lexical check,
// which is why the link target has to be checked too.
func TestExtractRefusesASymlinkEscapingTheDestination(t *testing.T) {
	outside := t.TempDir()
	victim := filepath.Join(outside, "pwned")
	dest := t.TempDir()

	raw := tarball(t,
		[]*tar.Header{
			{Name: "repo-sha/link", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0o777},
			{Name: "repo-sha/link/pwned", Typeflag: tar.TypeReg, Mode: 0o644},
		},
		[]string{"", "owned"},
	)

	err := extractTarGz(bytes.NewReader(raw), dest)
	require.Error(t, err, "a link escaping the extraction directory must be refused")
	assert.Contains(t, err.Error(), "outside the extraction directory")

	_, statErr := os.Stat(victim)
	assert.True(t, os.IsNotExist(statErr), "nothing may be written outside the destination")
}

// The same attack with a relative traversal.
func TestExtractRefusesARelativeSymlinkEscape(t *testing.T) {
	dest := t.TempDir()
	raw := tarball(t,
		[]*tar.Header{{Name: "repo-sha/link", Typeflag: tar.TypeSymlink, Linkname: "../../..", Mode: 0o777}},
		[]string{""},
	)
	err := extractTarGz(bytes.NewReader(raw), dest)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside the extraction directory")
}

// A hard link out of the tree is the same attack without a symlink.
func TestExtractRefusesAnEscapingHardLink(t *testing.T) {
	dest := t.TempDir()
	raw := tarball(t,
		[]*tar.Header{{Name: "repo-sha/hard", Typeflag: tar.TypeLink, Linkname: "/etc/passwd"}},
		[]string{""},
	)
	err := extractTarGz(bytes.NewReader(raw), dest)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside the extraction directory")
}

// A link that stays inside is legitimate and still works: actions do ship them.
func TestExtractAllowsALinkInsideTheDestination(t *testing.T) {
	dest := t.TempDir()
	raw := tarball(t,
		[]*tar.Header{
			{Name: "repo-sha/real.js", Typeflag: tar.TypeReg, Mode: 0o644},
			{Name: "repo-sha/alias.js", Typeflag: tar.TypeSymlink, Linkname: "real.js", Mode: 0o777},
		},
		[]string{"module.exports = 1\n", ""},
	)
	require.NoError(t, extractTarGz(bytes.NewReader(raw), dest))

	body, err := os.ReadFile(filepath.Join(dest, "alias.js"))
	require.NoError(t, err)
	assert.Equal(t, "module.exports = 1\n", string(body))
}

// Devices and FIFOs have no business in an action tarball.
func TestExtractRefusesUnsupportedEntryTypes(t *testing.T) {
	dest := t.TempDir()
	raw := tarball(t,
		[]*tar.Header{{Name: "repo-sha/dev", Typeflag: tar.TypeFifo, Mode: 0o644}},
		[]string{""},
	)
	err := extractTarGz(bytes.NewReader(raw), dest)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported type")
}

// The link checks above are lexical: they read the tarball's own entries. This
// covers the link they cannot see -- one already on disk when extraction
// reaches that name, planted by another process or left by an earlier run. The
// open refuses to follow it, so the file outside keeps its contents.
func TestExtractRefusesToWriteThroughAPlantedSymlink(t *testing.T) {
	dest := t.TempDir()
	outside := filepath.Join(t.TempDir(), "hostfile")
	require.NoError(t, os.WriteFile(outside, []byte("original"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(dest, "config.json")))

	raw := tarball(t,
		[]*tar.Header{{Name: "repo-sha/config.json", Typeflag: tar.TypeReg, Mode: 0o644}},
		[]string{"owned"},
	)
	require.Error(t, extractTarGz(bytes.NewReader(raw), dest))

	body, err := os.ReadFile(outside)
	require.NoError(t, err)
	assert.Equal(t, "original", string(body), "the write must not have gone through the link")
}
