package exec

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShellCommandBuiltins(t *testing.T) {
	tests := []struct {
		shell string
		argv  []string
		path  string
	}{
		{"", []string{"bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "/t/s.sh"}, "/t/s.sh"},
		{"bash", []string{"bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "/t/s.sh"}, "/t/s.sh"},
		{"sh", []string{"sh", "-e", "/t/s.sh"}, "/t/s.sh"},
		{"python", []string{"python", "/t/s.py"}, "/t/s.py"},
		{"node", []string{"node", "/t/s.js"}, "/t/s.js"},
		{"BASH", []string{"bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "/t/s.sh"}, "/t/s.sh"},
	}
	for _, tt := range tests {
		t.Run("shell="+tt.shell, func(t *testing.T) {
			argv, path, err := shellCommand(tt.shell, "/t/s")
			require.NoError(t, err)
			assert.Equal(t, tt.argv, argv)
			assert.Equal(t, tt.path, path, "the script path in argv must be the path written")
			assert.Equal(t, tt.path, argv[len(argv)-1])
		})
	}
}

func TestShellCommandCustom(t *testing.T) {
	argv, path, err := shellCommand("perl {0}", "/t/s")
	require.NoError(t, err)
	assert.Equal(t, []string{"perl", "/t/s"}, argv)
	assert.Equal(t, "/t/s", path)

	argv, _, err = shellCommand("bash -c 'source {0}'", "/t/s")
	require.NoError(t, err)
	assert.Equal(t, []string{"bash", "-c", "'source", "/t/s'"}, argv)
}

func TestShellCommandUnsupported(t *testing.T) {
	for _, shell := range []string{"pwsh", "powershell", "cmd", "PWSH", "pwsh -File {0}"} {
		_, _, err := shellCommand(shell, "/t/s")
		require.Error(t, err, "shell %q", shell)
		assert.Contains(t, err.Error(), "unsupported: shell")
	}
}

func TestShellCommandCustomWithoutPlaceholder(t *testing.T) {
	// The Actions runner rejects this too: without {0} there is nowhere to put
	// the script path.
	_, _, err := shellCommand("perl -w", "/t/s")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "{0}")

	_, _, err = shellCommand("ruby", "/t/s")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported:")
}
