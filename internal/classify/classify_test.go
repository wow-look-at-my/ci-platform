package classify

import (
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

func fixedClassifier() *Classifier {
	at := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	return &Classifier{Now: func() time.Time { return at }}
}

// The output in this case is the real thing from the incident that started the
// project: a registry blob upload killed by a Cloudflare origin timeout.
func TestClassify_CloudflareTimeoutIsInfra(t *testing.T) {
	c := fixedClassifier()
	d := c.Classify(Signal{
		ExitCode: 1,
		Phase:    "run",
		Output:   "pushing blob to registry\nfailed: 524 : error code: 524\n",
	})
	require.Equal(t, model.ClassInfra, d.Class)
	assert.Equal(t, "cloudflare-524", d.Rule)
	assert.True(t, d.Confident)
	assert.Contains(t, d.Evidence, "524")
	assert.Contains(t, d.String(), "classified infra")
	assert.Equal(t, model.ConclusionInfraFailure, d.Class.Conclusion())
	assert.True(t, d.Class.Retryable())
}

func TestClassify_InfraRules(t *testing.T) {
	tests := []struct {
		name   string
		signal Signal
		rule   string
	}{
		{
			name:   "registry 5xx",
			signal: Signal{Output: "received unexpected HTTP status: 503 Service Unavailable"},
			rule:   "registry-5xx",
		},
		{
			name:   "image pull",
			signal: Signal{Output: "Error response from daemon: failed to resolve reference \"x\": toomanyrequests"},
			rule:   "image-pull-failure",
		},
		{
			name:   "dns",
			signal: Signal{Output: "dial tcp: lookup registry.example.com: no such host"},
			rule:   "dns-failure",
		},
		{
			name:   "tls handshake",
			signal: Signal{Output: "net/http: TLS handshake timeout"},
			rule:   "tls-handshake-timeout",
		},
		{
			name:   "connection reset",
			signal: Signal{Output: "read tcp 10.0.0.1:443: connection reset by peer"},
			rule:   "connection-reset",
		},
		{
			name:   "docker daemon",
			signal: Signal{Output: "Cannot connect to the Docker daemon at unix:///var/run/docker.sock."},
			rule:   "docker-daemon-unreachable",
		},
		{
			name:   "apt mirror",
			signal: Signal{Output: "E: Failed to fetch http://deb.debian.org/x.deb  503  Service Unavailable"},
			rule:   "package-mirror-5xx",
		},
		{
			name:   "control plane",
			signal: Signal{Output: "POST /runnerresolve/actions failed (HTTP Status: BadGateway)"},
			rule:   "control-plane-unreachable",
		},
		{
			name:   "disk",
			signal: Signal{Output: "write /tmp/x: no space left on device"},
			rule:   "disk-pressure",
		},
		{
			name:   "oom",
			signal: Signal{Output: "Out of memory: Killed process 1234 (node)"},
			rule:   "oom-killed",
		},
		{
			name:   "git transport",
			signal: Signal{Output: "error: RPC failed; curl 56 Recv failure\nfatal: early EOF"},
			rule:   "git-transport",
		},
	}
	c := fixedClassifier()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.signal.ExitCode = 1
			if tc.signal.Phase == "" {
				tc.signal.Phase = "run"
			}
			d := c.Classify(tc.signal)
			assert.Equal(t, model.ClassInfra, d.Class)
			assert.Equal(t, tc.rule, d.Rule)
			assert.NotEmpty(t, d.Reason)
			assert.True(t, d.Confident)
		})
	}
}

func TestClassify_ConfigRulesBeatInfraRules(t *testing.T) {
	c := fixedClassifier()
	// Text that would match an infra rule is still a config error when the
	// failure is an unresolvable action: retrying cannot fix a bad ref.
	d := c.Classify(Signal{
		Phase:  "action-fetch",
		Output: "Unable to resolve action actions/checkout@v99, repository not found\nconnection reset by peer",
	})
	require.Equal(t, model.ClassConfig, d.Class)
	assert.Equal(t, "unresolvable-action", d.Rule)
	assert.False(t, d.Class.Retryable())
	assert.Equal(t, model.ConclusionConfigError, d.Class.Conclusion())
}

func TestClassify_UnsupportedFeatureIsConfig(t *testing.T) {
	c := fixedClassifier()
	d := c.Classify(Signal{Phase: "setup", Output: `unsupported: jobs.build.steps[2].shell "pwsh" is not implemented`})
	require.Equal(t, model.ClassConfig, d.Class)
	assert.Equal(t, "unsupported-feature", d.Rule)
}

// The default must be user. Defaulting to infra would make every red build
// ambiguous, which is the failure this platform exists to remove.
func TestClassify_DefaultsToUserAndSaysItIsNotConfident(t *testing.T) {
	c := fixedClassifier()
	d := c.Classify(Signal{ExitCode: 2, Phase: "run", Output: "FAIL\tgithub.com/x/y\t0.4s\n--- FAIL: TestThing"})
	require.Equal(t, model.ClassUser, d.Class)
	assert.Equal(t, "default-exit-code", d.Rule)
	assert.False(t, d.Confident)
	assert.Contains(t, d.Reason, "exited 2")
	assert.False(t, d.Class.Retryable())
	assert.Equal(t, model.ConclusionFailure, d.Class.Conclusion())
}

func TestClassify_PlatformPhaseIsInfraEvenWithoutAMatchingRule(t *testing.T) {
	c := fixedClassifier()
	d := c.Classify(Signal{ExitCode: 1, Phase: "setup", Output: "something inscrutable happened"})
	require.Equal(t, model.ClassInfra, d.Class)
	assert.Equal(t, "platform-phase", d.Rule)
	assert.Contains(t, d.Reason, "runs no user commands")
}

func TestClassify_TimeoutOwnerDependsOnPhase(t *testing.T) {
	c := fixedClassifier()

	setup := c.Classify(Signal{TimedOut: true, Phase: "setup"})
	assert.Equal(t, model.ClassInfra, setup.Class)
	assert.Equal(t, "setup-timeout", setup.Rule)

	step := c.Classify(Signal{TimedOut: true, Phase: "run"})
	assert.Equal(t, model.ClassUser, step.Class)
	assert.Equal(t, "step-timeout", step.Rule)
}

func TestClassify_CancellationIsNotAFailure(t *testing.T) {
	c := fixedClassifier()
	d := c.Classify(Signal{Cancelled: true, Phase: "run", ExitCode: 130})
	assert.Equal(t, model.ClassNone, d.Class)
	assert.Equal(t, "cancelled", d.Rule)
}

func TestClassify_HTTPStatusIsDecisiveWithoutARuleMatch(t *testing.T) {
	c := fixedClassifier()

	d := c.Classify(Signal{Phase: "run", HTTPStatus: 502, Host: "registry.internal"})
	require.Equal(t, model.ClassInfra, d.Class)
	assert.Equal(t, "http-5xx", d.Rule)
	assert.Contains(t, d.Reason, "registry.internal")

	rl := c.Classify(Signal{Phase: "run", HTTPStatus: 429})
	assert.Equal(t, model.ClassInfra, rl.Class)
	assert.Equal(t, "http-429", rl.Rule)
	assert.Contains(t, rl.Reason, "the remote")

	// A 4xx that is not 429 is the caller's problem, so it falls through to
	// the user default rather than being excused as infra.
	notFound := c.Classify(Signal{Phase: "run", HTTPStatus: 404, ExitCode: 1})
	assert.Equal(t, model.ClassUser, notFound.Class)
}

func TestClassify_ErrorTextIsSearchedAlongsideOutput(t *testing.T) {
	c := fixedClassifier()
	d := c.Classify(Signal{Phase: "run", ExitCode: 1, Err: errors.New("dial tcp: i/o timeout")})
	assert.Equal(t, model.ClassInfra, d.Class)
	assert.Equal(t, "connection-reset", d.Rule)
}

func TestClassify_OperatorSuppliedRuleApplies(t *testing.T) {
	c := fixedClassifier()
	c.Extra = []Rule{{
		Name:    "site-local-proxy",
		Class:   model.ClassInfra,
		Reason:  "the site proxy rejected the request",
		Pattern: regexp.MustCompile(`PROXY_REJECT`),
	}}
	d := c.Classify(Signal{Phase: "run", ExitCode: 1, Output: "curl: PROXY_REJECT from edge"})
	require.Equal(t, model.ClassInfra, d.Class)
	assert.Equal(t, "site-local-proxy", d.Rule)
}

func TestClassify_ExtraRuleRespectsPhaseRestriction(t *testing.T) {
	c := fixedClassifier()
	c.Extra = []Rule{{
		Name:    "teardown-only",
		Class:   model.ClassInfra,
		Reason:  "teardown only",
		Pattern: regexp.MustCompile(`WIDGET`),
		Phases:  []string{"teardown"},
	}}
	// A phase-restricted rule must not fire in another phase.
	d := c.Classify(Signal{Phase: "run", ExitCode: 1, Output: "WIDGET exploded"})
	assert.Equal(t, model.ClassUser, d.Class)
}

func TestClassify_NilPatternRuleIsIgnored(t *testing.T) {
	c := fixedClassifier()
	c.Extra = []Rule{{Name: "broken", Class: model.ClassInfra, Reason: "x"}}
	d := c.Classify(Signal{Phase: "run", ExitCode: 1, Output: "anything"})
	assert.Equal(t, model.ClassUser, d.Class)
}

func TestClassify_UsesInjectedClock(t *testing.T) {
	c := fixedClassifier()
	d := c.Classify(Signal{Phase: "run", ExitCode: 1})
	assert.Equal(t, time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC), d.At)

	// The zero value works and stamps wall-clock time.
	var zero Classifier
	got := zero.Classify(Signal{Phase: "run", ExitCode: 1})
	assert.WithinDuration(t, time.Now(), got.At, time.Minute)
}

func TestDecisionString(t *testing.T) {
	d := Decision{Class: model.ClassInfra, Rule: "r", Reason: "because"}
	assert.Equal(t, `classified infra via rule "r": because`, d.String())

	d.Evidence = "the thing"
	assert.Contains(t, d.String(), "(matched: the thing)")
}

func TestExcerptCollapsesAndBounds(t *testing.T) {
	long := ""
	for range 100 {
		long += "abcdefghij "
	}
	got := excerpt(long+"NEEDLE"+long, len(long), len(long)+6)
	assert.LessOrEqual(t, len(got), maxEvidence+3)
	assert.Contains(t, got, "NEEDLE")
	assert.NotContains(t, got, "\n")

	// A match at the very start must not index before zero.
	assert.Equal(t, "hi", excerpt("hi", 0, 2))
}

func TestIsPlatformPhase(t *testing.T) {
	for _, p := range []string{"setup", "SETUP", "action-fetch", "teardown", "dispatch", "lease"} {
		assert.True(t, isPlatformPhase(p), p)
	}
	for _, p := range []string{"run", "", "checkout"} {
		assert.False(t, isPlatformPhase(p), p)
	}
}

func TestHostOr(t *testing.T) {
	assert.Equal(t, "h", hostOr("h", "fallback"))
	assert.Equal(t, "fallback", hostOr("", "fallback"))
}
