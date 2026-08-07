package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func complete() map[string]string {
	return map[string]string{
		"CIPLATFORM_PUBLIC_URL":       "https://ci.example.localhost",
		"CIPLATFORM_DATABASE_URL":     "/var/lib/ciplatform/ciplatform.db",
		"CIPLATFORM_WEBHOOK_SECRET":   "s3cret",
		"CIPLATFORM_APP_ID":           "12345",
		"CIPLATFORM_APP_PRIVATE_KEY":  "-----BEGIN RSA PRIVATE KEY-----\n",
		"CIPLATFORM_JOB_TOKEN_SECRET": "job-secret",
		"CIPLATFORM_RUNNER_TOKEN":     "runner-secret",
		"CIPLATFORM_OPERATOR_TOKEN":   "operator-secret-long-enough",
	}
}

func env(m map[string]string) Getenv {
	return func(k string) string { return m[k] }
}

func TestLoad_Complete(t *testing.T) {
	c, err := LoadFrom(env(complete()))
	require.NoError(t, err)

	assert.Equal(t, ":8080", c.Listen)
	assert.Equal(t, int64(12345), c.AppID)
	assert.Equal(t, "disk", c.BlobDriver)
	assert.Equal(t, 90*time.Second, c.LeaseTTL)
	assert.Equal(t, "https://api.github.com", c.GitHubAPIURL.String())
	assert.False(t, c.AllowEphemeralStore)
	assert.True(t, c.RequireForkApproval, "a fork PR is a stranger's code, so the gate is on by default")
}

// The runner token is stored on every runner host and sent to the control
// plane. Reusing the job-token signing key there would put the key that mints
// every job's credentials on every runner.
func TestLoad_RejectsAReusedSigningKey(t *testing.T) {
	m := complete()
	m["CIPLATFORM_RUNNER_TOKEN"] = m["CIPLATFORM_JOB_TOKEN_SECRET"]

	_, err := LoadFrom(env(m))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must differ from CIPLATFORM_JOB_TOKEN_SECRET")
}

// Every job container can route to the control plane, so the operator API's
// credential must not be one a job or a runner already holds, and must not be
// short enough to guess.
func TestLoad_OperatorTokenConstraints(t *testing.T) {
	tests := []struct {
		name, val, want string
	}{
		{"reused runner token", "runner-secret", "must differ from CIPLATFORM_RUNNER_TOKEN"},
		{"reused signing key", "job-secret", "must differ from CIPLATFORM_JOB_TOKEN_SECRET"},
		{"too short", "hunter2", "at least 16 are required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := complete()
			m["CIPLATFORM_OPERATOR_TOKEN"] = tc.val
			_, err := LoadFrom(env(m))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// One restart per missing variable is a waste of an operator's afternoon, so
// every problem is reported at once.
func TestLoad_ReportsEveryMissingValueAtOnce(t *testing.T) {
	_, err := LoadFrom(env(map[string]string{}))
	require.Error(t, err)

	var missing *MissingError
	require.ErrorAs(t, err, &missing)
	assert.GreaterOrEqual(t, len(missing.Problems), 5)

	msg := err.Error()
	for _, name := range []string{
		"CIPLATFORM_PUBLIC_URL", "CIPLATFORM_DATABASE_URL", "CIPLATFORM_WEBHOOK_SECRET",
		"CIPLATFORM_APP_ID", "CIPLATFORM_JOB_TOKEN_SECRET", "CIPLATFORM_RUNNER_TOKEN",
		"CIPLATFORM_OPERATOR_TOKEN",
	} {
		assert.Contains(t, msg, name)
	}
	// Each problem says what the value is for, not just that it is missing.
	assert.Contains(t, msg, "check runs cannot be written without App auth")
}

func TestLoad_RejectsAHostnameTheArtifactClientWillNotAccept(t *testing.T) {
	m := complete()
	m["CIPLATFORM_PUBLIC_URL"] = "https://ci.internal.example.com"

	_, err := LoadFrom(env(m))
	require.ErrorIs(t, err, ErrGHESHostname)
	assert.Contains(t, err.Error(), "GHESNotSupportedError")
}

func TestArtifactClientAccepts(t *testing.T) {
	accepted := []string{"github.com", "GITHUB.COM", "foo.ghe.com", "ci.example.localhost", "localhost.localhost"}
	for _, h := range accepted {
		assert.True(t, ArtifactClientAccepts(h), h)
	}
	rejected := []string{"ci.example.com", "localhost", "ghe.com.evil.net", "", "ci.internal"}
	for _, h := range rejected {
		assert.False(t, ArtifactClientAccepts(h), h)
	}
}

// A heartbeat slower than the lease means every running job loses its lease and
// gets requeued forever, which looks exactly like a broken platform.
func TestLoad_RejectsHeartbeatSlowerThanLease(t *testing.T) {
	m := complete()
	m["CIPLATFORM_LEASE_TTL"] = "10s"
	m["CIPLATFORM_HEARTBEAT_INTERVAL"] = "30s"

	_, err := LoadFrom(env(m))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be shorter than lease TTL")
}

// A value that is set but unparseable is a typo the operator needs to hear
// about, not a silent revert to the default.
func TestLoad_RejectsMalformedValuesRatherThanDefaulting(t *testing.T) {
	tests := []struct {
		name, key, val, want string
	}{
		{"bool", "CIPLATFORM_ALLOW_EPHEMERAL_STORE", "ture", "is not a boolean"},
		{"duration", "CIPLATFORM_LEASE_TTL", "90", "is not a duration"},
		{"negative duration", "CIPLATFORM_LEASE_TTL", "-5s", "must be positive"},
		{"bytes", "CIPLATFORM_CACHE_QUOTA", "lots", "is not a byte size"},
		{"int", "CIPLATFORM_APP_ID", "twelve", "is not a number"},
		{"enum", "CIPLATFORM_BLOB_DRIVER", "gcs", `is not one of disk, s3`},
		{"url", "CIPLATFORM_PUBLIC_URL", "not-a-url", "is not an absolute URL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := complete()
			m[tc.key] = tc.val
			_, err := LoadFrom(env(m))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestLoad_S3DriverRequiresItsSettings(t *testing.T) {
	m := complete()
	m["CIPLATFORM_BLOB_DRIVER"] = "s3"

	_, err := LoadFrom(env(m))
	require.Error(t, err)
	for _, name := range []string{"CIPLATFORM_S3_BUCKET", "CIPLATFORM_S3_ENDPOINT", "CIPLATFORM_S3_KEY_ID", "CIPLATFORM_S3_SECRET"} {
		assert.Contains(t, err.Error(), name)
	}

	m["CIPLATFORM_S3_BUCKET"] = "b"
	m["CIPLATFORM_S3_ENDPOINT"] = "https://s3.example.com"
	m["CIPLATFORM_S3_KEY_ID"] = "k"
	m["CIPLATFORM_S3_SECRET"] = "s"
	c, err := LoadFrom(env(m))
	require.NoError(t, err)
	assert.Equal(t, "s3", c.BlobDriver)
}

func TestLoad_PrivateKeyFromPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(path, []byte("-----BEGIN RSA PRIVATE KEY-----\nabc\n"), 0o600))

	m := complete()
	delete(m, "CIPLATFORM_APP_PRIVATE_KEY")
	m["CIPLATFORM_APP_PRIVATE_KEY_PATH"] = path

	c, err := LoadFrom(env(m))
	require.NoError(t, err)
	assert.Contains(t, string(c.AppPrivateKey), "BEGIN RSA PRIVATE KEY")
}

// An unreadable or empty key file must never yield an empty key that would
// produce unsigned JWTs and a fleet of 401s nobody can explain.
func TestLoad_PrivateKeyPathProblemsAreHardErrors(t *testing.T) {
	m := complete()
	delete(m, "CIPLATFORM_APP_PRIVATE_KEY")
	m["CIPLATFORM_APP_PRIVATE_KEY_PATH"] = "/nope/does/not/exist.pem"
	_, err := LoadFrom(env(m))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not be read")

	empty := filepath.Join(t.TempDir(), "empty.pem")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))
	m["CIPLATFORM_APP_PRIVATE_KEY_PATH"] = empty
	_, err = LoadFrom(env(m))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is empty")
}

func TestParseBytes(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"1024", 1024},
		{"1KiB", 1 << 10},
		{"2MiB", 2 << 20},
		{"3GiB", 3 << 30},
		{"1TiB", 1 << 40},
		{"1KB", 1000},
		{"1MB", 1000 * 1000},
		{"1GB", 1000 * 1000 * 1000},
		{"1TB", 1000 * 1000 * 1000 * 1000},
		{"5G", 5 << 30},
		{"512B", 512},
		{"1.5GiB", 1610612736},
	}
	for _, tc := range tests {
		got, err := ParseBytes(tc.in)
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
	for _, bad := range []string{"", "lots", "-1MiB", "MiB", "1.2.3GiB"} {
		_, err := ParseBytes(bad)
		assert.Error(t, err, bad)
	}
}

func TestLoad_UsesProcessEnvironment(t *testing.T) {
	for k, v := range complete() {
		t.Setenv(k, v)
	}
	c, err := Load()
	require.NoError(t, err)
	assert.Equal(t, int64(12345), c.AppID)
}

func TestMissingErrorMessage(t *testing.T) {
	e := &MissingError{Problems: []string{"A is required", "B is required"}}
	assert.Contains(t, e.Error(), "configuration is incomplete")
	assert.Contains(t, e.Error(), "- A is required")
	assert.Contains(t, e.Error(), "- B is required")
}

func TestValidate_RequiresPublicURL(t *testing.T) {
	require.ErrorContains(t, (&Config{}).Validate(), "public URL is not set")
}
