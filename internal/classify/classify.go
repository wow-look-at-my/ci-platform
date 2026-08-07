// Package classify decides whether a failure was the user's, the
// infrastructure's, or the workflow author's.
//
// This is the platform's core claim: a red build means your code is broken. If
// the classifier gets this wrong in the permissive direction it hands back the
// exact GitHub Actions experience the platform was built to replace, where a
// Cloudflare 524 during a registry push is indistinguishable from a failing
// test. So every decision is recorded with the evidence that produced it, and
// the decisions are surfaced, not just logged.
package classify

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// Decision is one classification, with the evidence behind it. The Reason is
// written to be read by an operator at 2am: it names the rule and quotes the
// text that matched.
type Decision struct {
	Class model.FailureClass `json:"class"`
	// Rule is the identifier of the matching rule, e.g. "registry-5xx".
	Rule string `json:"rule"`
	// Reason is a complete sentence explaining the decision.
	Reason string `json:"reason"`
	// Evidence is the matched excerpt, truncated and masked.
	Evidence string `json:"evidence,omitempty"`
	// Confident is false when the classifier fell back to a default rather than
	// matching a rule. A non-confident infra call is never made: the default is
	// always user, because wrongly retrying a user failure wastes an operator's
	// time while wrongly failing an infra error destroys trust in red builds.
	Confident bool      `json:"confident"`
	At        time.Time `json:"at"`
}

// String renders the decision as the one line that goes in the job log.
func (d Decision) String() string {
	s := fmt.Sprintf("classified %s via rule %q: %s", d.Class, d.Rule, d.Reason)
	if d.Evidence != "" {
		s += fmt.Sprintf(" (matched: %s)", d.Evidence)
	}
	return s
}

// Signal is everything known about a failure at classification time. Every
// field is optional; the classifier uses what it has.
type Signal struct {
	// ExitCode of the failing process, if a process ran at all.
	ExitCode int
	// Output is the tail of the step's combined output.
	Output string
	// Err is a Go error from the platform's own machinery, e.g. a docker API
	// call or an HTTP request to the control plane.
	Err error
	// Phase is where the failure happened: "setup", "checkout", "action-fetch",
	// "run", "upload", "teardown". Failures outside "run" are much more likely
	// to be ours than the user's.
	Phase string
	// HTTPStatus is set when the failure was an HTTP response.
	HTTPStatus int
	// Host is the remote host involved, used only for the reason text.
	Host string
	// TimedOut marks a step or job that hit its own timeout.
	TimedOut bool
	// Cancelled marks a step stopped by an external cancellation.
	Cancelled bool
}

// rule is one pattern-based classification rule.
type rule struct {
	name    string
	class   model.FailureClass
	reason  string
	pattern *regexp.Regexp
	// phases, when non-empty, restricts the rule to those phases.
	phases []string
}

// infraRules are the known-transient failures the platform recognizes out of
// the box. Each is drawn from a failure that actually happens in practice, not
// from imagination: registry 5xx and Cloudflare's 524, image pull timeouts, DNS
// resolution failures, TLS handshake timeouts, an unreachable docker daemon,
// and 5xx from apt/npm mirrors and proxies.
var infraRules = []rule{
	{
		name:    "cloudflare-524",
		class:   model.ClassInfra,
		reason:  "the remote returned HTTP 524, which is Cloudflare timing out waiting for its origin, not a response from the service itself",
		pattern: regexp.MustCompile(`(?i)\b524\b.*(error code|timeout)|error code:\s*524|failed:\s*524`),
	},
	{
		name:    "registry-5xx",
		class:   model.ClassInfra,
		reason:  "the container registry returned a 5xx, so the push or pull failed on the registry's side",
		pattern: regexp.MustCompile(`(?i)(received unexpected HTTP status:\s*5\d\d|registry.*\b5\d\d\b|blob upload.*(failed|unknown)|unexpected status.*from.*(PUT|POST).*\b5\d\d\b)`),
	},
	{
		name:    "image-pull-failure",
		class:   model.ClassInfra,
		reason:  "the image could not be pulled, which is a registry or network failure rather than a defect in the workflow",
		pattern: regexp.MustCompile(`(?i)(failed to (pull|resolve) (image|reference)|error pulling image|pull access denied.*(timeout|temporarily)|manifest unknown: .*(retry|temporar)|toomanyrequests|429 Too Many Requests)`),
	},
	{
		name:    "dns-failure",
		class:   model.ClassInfra,
		reason:  "a hostname failed to resolve, so the network could not be reached",
		pattern: regexp.MustCompile(`(?i)(no such host|temporary failure in name resolution|could not resolve host|name or service not known|EAI_AGAIN|dns lookup.*failed|server misbehaving)`),
	},
	{
		name:    "tls-handshake-timeout",
		class:   model.ClassInfra,
		reason:  "the TLS handshake timed out before the request was sent, so nothing the workflow did could have caused it",
		pattern: regexp.MustCompile(`(?i)(TLS handshake timeout|tls: handshake failure|remote error: tls: internal error)`),
	},
	{
		name:    "connection-reset",
		class:   model.ClassInfra,
		reason:  "the connection was reset or refused mid-transfer, which is a network fault",
		pattern: regexp.MustCompile(`(?i)(connection reset by peer|connection refused|broken pipe|unexpected EOF|EOF occurred in violation of protocol|read: connection timed out|i/o timeout|network is unreachable|no route to host)`),
	},
	{
		name:    "docker-daemon-unreachable",
		class:   model.ClassInfra,
		reason:  "the docker daemon was unreachable, so the sandbox itself failed rather than the command inside it",
		pattern: regexp.MustCompile(`(?i)(cannot connect to the docker daemon|is the docker daemon running|docker daemon.*not running|dial unix /var/run/docker\.sock|error during connect:.*docker)`),
	},
	{
		name:    "package-mirror-5xx",
		class:   model.ClassInfra,
		reason:  "a package mirror or proxy returned a 5xx, so the dependency fetch failed upstream of the workflow",
		pattern: regexp.MustCompile(`(?i)(E: Failed to fetch.*\b(50\d|429)\b|Hash Sum mismatch|npm ERR!.*(ETIMEDOUT|ECONNRESET|EAI_AGAIN|registry error|503)|Could not resolve dependencies.*(502|503|504)|502 Bad Gateway|503 Service (Unavailable|Temporarily)|504 Gateway Time-?out|The requested URL returned error: 5\d\d)`),
	},
	{
		name:    "control-plane-unreachable",
		class:   model.ClassInfra,
		reason:  "the runner could not reach the control plane, which is a platform failure and never a property of the workflow",
		pattern: regexp.MustCompile(`(?i)(runnerresolve.*failed|control plane unreachable|BadGateway|ServiceUnavailable|GatewayTimeout)`),
	},
	{
		name:    "disk-pressure",
		class:   model.ClassInfra,
		reason:  "the host ran out of disk or inodes, which is a capacity failure of the runner rather than of the build",
		pattern: regexp.MustCompile(`(?i)(no space left on device|write error: no space|cannot create temp file.*no space|disk quota exceeded)`),
	},
	{
		name:    "oom-killed",
		class:   model.ClassInfra,
		reason:  "the kernel OOM-killed the process, which is a runner capacity failure rather than a defect the build can fix",
		pattern: regexp.MustCompile(`(?i)(killed process .* \(.*\) total-vm|Out of memory: Killed process|OOMKilled|signal: killed.*oom)`),
	},
	{
		name:    "git-transport",
		class:   model.ClassInfra,
		reason:  "the git transport failed mid-transfer, which is a network fault rather than a bad revision",
		pattern: regexp.MustCompile(`(?i)(RPC failed; (curl|HTTP) \d+|early EOF|index-pack failed|the remote end hung up unexpectedly|fetch-pack: unexpected disconnect|error: RPC failed)`),
	},
}

// configRules recognize a workflow that cannot work, no matter how many times
// it is retried. These are separated from user failures because retrying them
// is pointless and because the fix is in the YAML, not the code.
var configRules = []rule{
	{
		name:    "unresolvable-action",
		class:   model.ClassConfig,
		reason:  "the action reference could not be resolved, so the workflow names something that does not exist",
		pattern: regexp.MustCompile(`(?i)(unable to resolve action|action .* not found|reference .* does not exist|repository .* not found.*action)`),
		phases:  []string{"action-fetch"},
	},
	{
		name:    "unsupported-feature",
		class:   model.ClassConfig,
		reason:  "the workflow uses a feature this platform does not implement, and the run was failed rather than silently ignoring the key",
		pattern: regexp.MustCompile(`unsupported:`),
	},
	{
		name:    "workflow-parse-error",
		class:   model.ClassConfig,
		reason:  "the workflow file could not be parsed or validated",
		pattern: regexp.MustCompile(`(?i)(workflow (parse|validation) error|invalid workflow file|yaml: )`),
	},
	{
		name:    "expression-error",
		class:   model.ClassConfig,
		reason:  "an expression could not be evaluated, which is a defect in the workflow rather than in the code it builds",
		pattern: regexp.MustCompile(`(?i)(unrecognized (named-value|function)|invalid expression|expression error:)`),
	},
}

// Classifier applies the rule set. The zero value is ready to use.
type Classifier struct {
	// Extra rules are appended after the built-ins, letting an operator teach
	// the platform about a failure mode specific to their environment.
	Extra []Rule
	// Now is injectable for tests.
	Now func() time.Time
}

// Rule is an operator-supplied classification rule.
type Rule struct {
	Name    string
	Class   model.FailureClass
	Reason  string
	Pattern *regexp.Regexp
	Phases  []string
}

func (c *Classifier) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// maxEvidence bounds how much matched text is quoted back into the reason.
const maxEvidence = 200

// Classify decides. It never returns ClassNone for a failure signal: every
// failure gets an owner.
func (c *Classifier) Classify(s Signal) Decision {
	now := c.now()

	// A cancellation is not a failure and must not be classified as one; the
	// caller records the CancelReason instead.
	if s.Cancelled {
		return Decision{
			Class: model.ClassNone, Rule: "cancelled", Confident: true, At: now,
			Reason: "the step was cancelled, so no failure is attributed to it",
		}
	}

	// A timeout's owner depends on where it happened. A step that ran for its
	// full budget is the user's; a setup phase that never finished is ours.
	if s.TimedOut {
		if isPlatformPhase(s.Phase) {
			return Decision{
				Class: model.ClassInfra, Rule: "setup-timeout", Confident: true, At: now,
				Reason: fmt.Sprintf("the %s phase exceeded its timeout before any user command ran, so the platform failed to prepare the job", s.Phase),
			}
		}
		return Decision{
			Class: model.ClassUser, Rule: "step-timeout", Confident: true, At: now,
			Reason: "the command exceeded the timeout the workflow set for it",
		}
	}

	haystack := s.Output
	if s.Err != nil {
		haystack += "\n" + s.Err.Error()
	}

	// Config errors are checked first: a workflow that cannot work must not be
	// retried as though the network were at fault.
	for _, r := range configRules {
		if d, ok := match(r, s, haystack, now); ok {
			return d
		}
	}
	for _, r := range infraRules {
		if d, ok := match(r, s, haystack, now); ok {
			return d
		}
	}
	for _, e := range c.Extra {
		if d, ok := match(rule{e.Name, e.Class, e.Reason, e.Pattern, e.Phases}, s, haystack, now); ok {
			return d
		}
	}

	// An HTTP status is decisive even when no rule text matched, because the
	// status code alone says whose side the failure was on.
	if s.HTTPStatus >= 500 {
		return Decision{
			Class: model.ClassInfra, Rule: "http-5xx", Confident: true, At: now,
			Reason: fmt.Sprintf("%s responded HTTP %d, a server-side failure", hostOr(s.Host, "the remote"), s.HTTPStatus),
		}
	}
	if s.HTTPStatus == 429 {
		return Decision{
			Class: model.ClassInfra, Rule: "http-429", Confident: true, At: now,
			Reason: fmt.Sprintf("%s rate-limited the request with HTTP 429", hostOr(s.Host, "the remote")),
		}
	}

	// A failure during a phase the user never wrote a command for is ours by
	// construction: there is no user code in "setup".
	if isPlatformPhase(s.Phase) {
		return Decision{
			Class: model.ClassInfra, Rule: "platform-phase", Confident: true, At: now,
			Reason: fmt.Sprintf("the failure happened during the %s phase, which runs no user commands, so it is the platform's", s.Phase),
		}
	}

	// Default: the user's command exited non-zero and nothing suggests
	// otherwise. Defaulting to infra here would make every red build
	// ambiguous, which is the failure mode this platform exists to remove.
	return Decision{
		Class: model.ClassUser, Rule: "default-exit-code", Confident: false, At: now,
		Reason: fmt.Sprintf("the command exited %d and no known infrastructure failure was found in its output", s.ExitCode),
	}
}

func match(r rule, s Signal, haystack string, now time.Time) (Decision, bool) {
	if len(r.phases) > 0 && !containsFold(r.phases, s.Phase) {
		return Decision{}, false
	}
	if r.pattern == nil {
		return Decision{}, false
	}
	loc := r.pattern.FindStringIndex(haystack)
	if loc == nil {
		return Decision{}, false
	}
	return Decision{
		Class:     r.class,
		Rule:      r.name,
		Reason:    r.reason,
		Evidence:  excerpt(haystack, loc[0], loc[1]),
		Confident: true,
		At:        now,
	}, true
}

// isPlatformPhase reports whether the phase runs no user-authored commands.
func isPlatformPhase(phase string) bool {
	switch strings.ToLower(phase) {
	case "setup", "action-fetch", "teardown", "dispatch", "lease":
		return true
	}
	return false
}

func containsFold(hay []string, needle string) bool {
	for _, h := range hay {
		if strings.EqualFold(h, needle) {
			return true
		}
	}
	return false
}

func hostOr(host, fallback string) string {
	if host == "" {
		return fallback
	}
	return host
}

// excerpt quotes the matched text with a little surrounding context, collapsed
// to one line and bounded so a matched megabyte of output cannot end up in a
// check run summary.
func excerpt(s string, start, end int) string {
	const pad = 40
	lo := start - pad
	if lo < 0 {
		lo = 0
	}
	hi := end + pad
	if hi > len(s) {
		hi = len(s)
	}
	out := strings.TrimSpace(strings.Join(strings.Fields(s[lo:hi]), " "))
	if len(out) > maxEvidence {
		out = out[:maxEvidence] + "..."
	}
	return out
}
