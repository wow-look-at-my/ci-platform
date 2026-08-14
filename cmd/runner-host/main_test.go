package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidate_NamesEveryMissingSetting(t *testing.T) {
	c := config{runners: 1}
	err := c.validate()
	require.Error(t, err, "a supervisor with no control plane must fail at startup, never idle")
	assert.Contains(t, err.Error(), "-url")
	// There is no credential to configure: the key is generated.
	assert.NotContains(t, err.Error(), "token")
}

func TestValidate_DefaultsTheNameToTheHostname(t *testing.T) {
	c := config{url: "http://192.168.1.84:8080", labels: "self-hosted", runners: 2}
	require.NoError(t, c.validate())

	host, err := os.Hostname()
	require.NoError(t, err)
	assert.Equal(t, host, c.name)
}

// The name prefixes every container and volume this host owns, so a value that
// is not a legal name has to be refused where it is typed.
func TestValidate_RejectsANameThatCannotNameAContainer(t *testing.T) {
	for _, name := range []string{"my host", "a/b", "host:1"} {
		c := config{url: "u", labels: "l", runners: 1, name: name}
		require.ErrorContains(t, c.validate(), "cannot contain")
	}
}

func TestValidate_RejectsAnEmptyPool(t *testing.T) {
	c := config{url: "u", labels: "l", runners: 0, name: "host"}
	require.ErrorContains(t, c.validate(), "-runners must be at least 1")
}

func TestSplitLabels(t *testing.T) {
	assert.Equal(t, []string{"self-hosted", "linux"}, splitLabels(" self-hosted, linux ,"))
	assert.Nil(t, splitLabels("  "))
}

func TestEnvOr(t *testing.T) {
	assert.Equal(t, "fallback", envOr("CI_RUNNER_HOST_TEST_UNSET", "fallback"))
	t.Setenv("CI_RUNNER_HOST_TEST_SET", "value")
	assert.Equal(t, "value", envOr("CI_RUNNER_HOST_TEST_SET", "fallback"))
}

func TestVersionFlagExitsWithoutTouchingDocker(t *testing.T) {
	require.NoError(t, run(t.Context(), []string{"-version"}))
}
