// Package config loads the control plane's configuration from the environment.
//
// Every required value is required: there is no placeholder mode, no "not
// configured yet" path, and no fallback that lets the process start in a state
// where it would report success without doing the work. A missing value fails
// at startup, naming the variable and what it is for.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/operatorauth"
)

// Config is the control plane's resolved configuration.
type Config struct {
	// PublicURL is the base URL browsers and runners reach this instance on.
	// Its host must satisfy the artifact client's isGhes() test, or
	// actions/upload-artifact@v4 refuses to run; see docs/deviations.md.
	PublicURL *url.URL
	Listen    string

	DatabaseURL string
	// AllowEphemeralStore permits the in-memory store, which loses everything
	// on restart. Off by default so a production deploy cannot get it silently.
	AllowEphemeralStore bool

	GitHubAPIURL   *url.URL
	AppID          int64
	AppPrivateKey  []byte
	WebhookSecret  string
	JobTokenSecret []byte
	// RunnerToken authenticates runner agents. It is deliberately NOT the job
	// token signing key: the runner holds this value on disk and sends it to
	// the control plane, so reusing the signing key would put the key that
	// mints every job's credentials on every runner host.
	RunnerToken string
	// OperatorToken gates the REST API and the UI. Every job container can
	// route to this instance -- it has to, to upload artifacts -- so an
	// ungated /api/v1 is reachable from inside any workflow, including a fork
	// PR's.
	OperatorToken string
	// RequireForkApproval holds a fork PR's jobs until a maintainer approves.
	// On by default: a fork PR is a stranger's code on your runners.
	RequireForkApproval bool

	BlobDriver string // disk | s3
	BlobRoot   string
	S3Endpoint string
	S3Bucket   string
	S3Region   string
	S3KeyID    string
	S3Secret   string

	OIDCKeyPath string

	LeaseTTL          time.Duration
	HeartbeatInterval time.Duration
	SetupTimeout      time.Duration
	RunTimeout        time.Duration
	CheckCoalesce     time.Duration

	ArtifactRetention time.Duration
	ArtifactQuota     int64
	CacheQuota        int64
}

// MissingError lists every configuration problem at once, so an operator fixes
// them in one pass instead of restarting once per missing variable.
type MissingError struct{ Problems []string }

func (e *MissingError) Error() string {
	return "configuration is incomplete:\n  - " + strings.Join(e.Problems, "\n  - ")
}

// Getenv is the environment lookup, injectable for tests.
type Getenv func(string) string

// Load reads configuration from the process environment.
func Load() (*Config, error) { return LoadFrom(os.Getenv) }

// LoadFrom reads configuration from an arbitrary lookup.
func LoadFrom(env Getenv) (*Config, error) {
	l := &loader{env: env}
	c := &Config{
		Listen:              l.str("CIPLATFORM_LISTEN", ":8080"),
		DatabaseURL:         l.required("CIPLATFORM_DATABASE_URL", "the Postgres DSN holding runs, jobs, and the durable queue"),
		AllowEphemeralStore: l.bool("CIPLATFORM_ALLOW_EPHEMERAL_STORE", false),
		WebhookSecret:       l.required("CIPLATFORM_WEBHOOK_SECRET", "the shared secret GitHub signs webhook deliveries with"),
		RunnerToken:         l.required("CIPLATFORM_RUNNER_TOKEN", "the shared secret runner agents authenticate with; it must differ from the job token signing key"),
		OperatorToken:       l.required("CIPLATFORM_OPERATOR_TOKEN", "the credential for the REST API and the web UI; without it every job container could read every repository's logs"),
		RequireForkApproval: l.bool("CIPLATFORM_REQUIRE_FORK_APPROVAL", true),
		BlobDriver:          l.enum("CIPLATFORM_BLOB_DRIVER", "disk", "disk", "s3"),
		BlobRoot:            l.str("CIPLATFORM_BLOB_ROOT", "/var/lib/ciplatform/blobs"),
		S3Endpoint:          l.str("CIPLATFORM_S3_ENDPOINT", ""),
		S3Bucket:            l.str("CIPLATFORM_S3_BUCKET", ""),
		S3Region:            l.str("CIPLATFORM_S3_REGION", "us-east-1"),
		S3KeyID:             l.str("CIPLATFORM_S3_KEY_ID", ""),
		S3Secret:            l.str("CIPLATFORM_S3_SECRET", ""),
		OIDCKeyPath:         l.str("CIPLATFORM_OIDC_KEY_PATH", "/var/lib/ciplatform/oidc"),
		LeaseTTL:            l.duration("CIPLATFORM_LEASE_TTL", 90*time.Second),
		HeartbeatInterval:   l.duration("CIPLATFORM_HEARTBEAT_INTERVAL", 20*time.Second),
		SetupTimeout:        l.duration("CIPLATFORM_SETUP_TIMEOUT", 10*time.Minute),
		RunTimeout:          l.duration("CIPLATFORM_RUN_TIMEOUT", 6*time.Hour),
		CheckCoalesce:       l.duration("CIPLATFORM_CHECK_COALESCE", 2*time.Second),
		ArtifactRetention:   l.duration("CIPLATFORM_ARTIFACT_RETENTION", 90*24*time.Hour),
		ArtifactQuota:       l.bytes("CIPLATFORM_ARTIFACT_QUOTA", 50<<30),
		CacheQuota:          l.bytes("CIPLATFORM_CACHE_QUOTA", 10<<30),
	}

	c.PublicURL = l.url("CIPLATFORM_PUBLIC_URL", "", "the base URL runners and browsers reach this instance on")
	c.GitHubAPIURL = l.url("CIPLATFORM_GITHUB_API_URL", "https://api.github.com", "")
	c.AppID = l.int64("CIPLATFORM_APP_ID", "the GitHub App's numeric ID; check runs cannot be written without App auth")
	c.AppPrivateKey = l.file("CIPLATFORM_APP_PRIVATE_KEY", "CIPLATFORM_APP_PRIVATE_KEY_PATH",
		"the GitHub App's PEM private key, used to mint installation tokens")
	c.JobTokenSecret = []byte(l.required("CIPLATFORM_JOB_TOKEN_SECRET", "the signing secret for per-job scoped tokens"))

	if c.BlobDriver == "s3" {
		for _, req := range []struct{ name, val, why string }{
			{"CIPLATFORM_S3_BUCKET", c.S3Bucket, "the bucket holding logs, artifacts, and cache"},
			{"CIPLATFORM_S3_ENDPOINT", c.S3Endpoint, "the S3-compatible endpoint"},
			{"CIPLATFORM_S3_KEY_ID", c.S3KeyID, "the access key id"},
			{"CIPLATFORM_S3_SECRET", c.S3Secret, "the secret access key"},
		} {
			if req.val == "" {
				l.missf("%s is required when CIPLATFORM_BLOB_DRIVER=s3 (%s)", req.name, req.why)
			}
		}
	}

	if len(l.problems) > 0 {
		return nil, &MissingError{Problems: l.problems}
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// ErrGHESHostname is returned when the public URL would make the artifact
// client refuse to run.
var ErrGHESHostname = errors.New("config: public URL host must be github.com, or end in .ghe.com or .localhost")

// Validate checks the cross-field constraints.
func (c *Config) Validate() error {
	if c.PublicURL == nil {
		return errors.New("config: public URL is not set")
	}
	if !ArtifactClientAccepts(c.PublicURL.Hostname()) {
		return fmt.Errorf("%w: %q would make actions/upload-artifact@v4 throw GHESNotSupportedError "+
			"before issuing a request (see docs/deviations.md)", ErrGHESHostname, c.PublicURL.Hostname())
	}
	// The runner token travels to every runner host; the signing key must not.
	if c.RunnerToken != "" && c.RunnerToken == string(c.JobTokenSecret) {
		return errors.New("config: CIPLATFORM_RUNNER_TOKEN must differ from CIPLATFORM_JOB_TOKEN_SECRET; " +
			"the runner token is stored on every runner host, and reusing the signing key there would let " +
			"anyone holding it mint a job token for any repository")
	}
	// The operator token is typed into a browser and pasted into scripts. Each
	// of the other two reaches somewhere it must not: the runner token is on
	// every runner host, and the signing key mints every job's credentials.
	if c.OperatorToken != "" {
		for _, other := range []struct{ name, val string }{
			{"CIPLATFORM_RUNNER_TOKEN", c.RunnerToken},
			{"CIPLATFORM_JOB_TOKEN_SECRET", string(c.JobTokenSecret)},
		} {
			if c.OperatorToken == other.val {
				return fmt.Errorf("config: CIPLATFORM_OPERATOR_TOKEN must differ from %s; "+
					"sharing one value means anything holding either one holds both", other.name)
			}
		}
		if len(c.OperatorToken) < operatorauth.MinTokenLen {
			return fmt.Errorf("config: CIPLATFORM_OPERATOR_TOKEN is %d characters; at least %d are required, "+
				"because it is the only thing protecting every repository's logs and artifacts",
				len(c.OperatorToken), operatorauth.MinTokenLen)
		}
	}
	if c.HeartbeatInterval >= c.LeaseTTL {
		return fmt.Errorf("config: heartbeat interval %s must be shorter than lease TTL %s, "+
			"or every running job loses its lease and is requeued", c.HeartbeatInterval, c.LeaseTTL)
	}
	return nil
}

// ArtifactClientAccepts mirrors isGhes() in @actions/artifact: the client
// refuses to run unless the server hostname is github.com, ends with .ghe.com,
// or ends with .localhost. Reproduced here so the failure surfaces at startup
// rather than inside somebody's first artifact upload.
func ArtifactClientAccepts(hostname string) bool {
	h := strings.ToUpper(strings.TrimRight(hostname, " "))
	return h == "GITHUB.COM" || strings.HasSuffix(h, ".GHE.COM") || strings.HasSuffix(h, ".LOCALHOST")
}

type loader struct {
	env      Getenv
	problems []string
}

func (l *loader) missf(format string, args ...any) {
	l.problems = append(l.problems, fmt.Sprintf(format, args...))
}

func (l *loader) str(name, def string) string {
	if v := l.env(name); v != "" {
		return v
	}
	return def
}

func (l *loader) required(name, why string) string {
	v := l.env(name)
	if v == "" {
		l.missf("%s is required (%s)", name, why)
	}
	return v
}

func (l *loader) enum(name, def string, allowed ...string) string {
	v := l.str(name, def)
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	l.missf("%s=%q is not one of %s", name, v, strings.Join(allowed, ", "))
	return def
}

// bool rejects a value that is set but unparseable rather than reverting to the
// default: RETRIES=ture must not silently read as false.
func (l *loader) bool(name string, def bool) bool {
	raw := l.env(name)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		l.missf("%s=%q is not a boolean", name, raw)
		return def
	}
	return v
}

func (l *loader) duration(name string, def time.Duration) time.Duration {
	raw := l.env(name)
	if raw == "" {
		return def
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		l.missf("%s=%q is not a duration (try 30s, 5m, 2h)", name, raw)
		return def
	}
	if v <= 0 {
		l.missf("%s=%q must be positive", name, raw)
		return def
	}
	return v
}

func (l *loader) bytes(name string, def int64) int64 {
	raw := l.env(name)
	if raw == "" {
		return def
	}
	v, err := ParseBytes(raw)
	if err != nil {
		l.missf("%s=%q is not a byte size (try 512MiB, 10GiB)", name, raw)
		return def
	}
	return v
}

func (l *loader) int64(name, why string) int64 {
	raw := l.env(name)
	if raw == "" {
		l.missf("%s is required (%s)", name, why)
		return 0
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		l.missf("%s=%q is not a number", name, raw)
		return 0
	}
	return v
}

func (l *loader) url(name, def, why string) *url.URL {
	raw := l.str(name, def)
	if raw == "" {
		l.missf("%s is required (%s)", name, why)
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		l.missf("%s=%q is not an absolute URL", name, raw)
		return nil
	}
	return u
}

// file reads a value given either inline or as a path. A path that cannot be
// read is a hard error, never an empty value that produces an unsigned JWT.
func (l *loader) file(inlineName, pathName, why string) []byte {
	if v := l.env(inlineName); v != "" {
		return []byte(v)
	}
	path := l.env(pathName)
	if path == "" {
		l.missf("%s or %s is required (%s)", inlineName, pathName, why)
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		l.missf("%s=%q could not be read: %v", pathName, path, err)
		return nil
	}
	if len(b) == 0 {
		l.missf("%s=%q is empty", pathName, path)
		return nil
	}
	return b
}

// ParseBytes accepts plain byte counts and IEC/SI suffixes.
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty size")
	}
	units := []struct {
		suffix string
		mult   int64
	}{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40},
		{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000}, {"TB", 1000 * 1000 * 1000 * 1000},
		{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40},
		{"B", 1},
	}
	up := strings.ToUpper(s)
	for _, u := range units {
		if strings.HasSuffix(up, strings.ToUpper(u.suffix)) {
			num := strings.TrimSpace(s[:len(s)-len(u.suffix)])
			v, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, fmt.Errorf("bad size %q", s)
			}
			if v < 0 {
				return 0, fmt.Errorf("negative size %q", s)
			}
			return int64(v * float64(u.mult)), nil
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return v, nil
}
