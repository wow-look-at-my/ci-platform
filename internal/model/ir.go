package model

import "time"

// The IR is the parser's output and the scheduler's and executor's input. It is
// deliberately not a GitHub Actions AST: the GHA YAML frontend is one frontend
// among several (see docs/format-trajectory.md), and everything downstream of
// the parser knows only these types.
//
// Expressions survive into the IR unevaluated, as Expr, because most of them
// cannot be evaluated until earlier jobs have produced outputs.

// Expr is a string that may contain ${{ }} interpolation. An Expr whose Raw
// contains no "${{" is a literal and evaluates to itself.
type Expr struct {
	Raw string `json:"raw"`
}

// NewExpr wraps a raw string.
func NewExpr(s string) Expr { return Expr{Raw: s} }

// IsLiteral reports whether evaluation is a no-op.
func (e Expr) IsLiteral() bool {
	for i := 0; i+2 < len(e.Raw); i++ {
		if e.Raw[i] == '$' && e.Raw[i+1] == '{' && e.Raw[i+2] == '{' {
			return false
		}
	}
	return true
}

// String returns the raw text.
func (e Expr) String() string { return e.Raw }

// Empty reports whether the expression has no text at all.
func (e Expr) Empty() bool { return e.Raw == "" }

// Workflow is one parsed workflow file.
type Workflow struct {
	// Path is the repo-relative path, e.g. ".github/workflows/ci.yml".
	Path string `json:"path"`
	// Name is the workflow's display name, defaulting to Path.
	Name string `json:"name"`
	// RunName is the optional run-name: template.
	RunName Expr `json:"run_name,omitempty"`

	On          Triggers          `json:"on"`
	Env         map[string]Expr   `json:"env,omitempty"`
	Defaults    Defaults          `json:"defaults,omitempty"`
	Concurrency *Concurrency      `json:"concurrency,omitempty"`
	Permissions *Permissions      `json:"permissions,omitempty"`
	Jobs        map[string]*JobIR `json:"jobs"`
	// JobOrder preserves declaration order for stable display and stable
	// matrix-leg numbering; Go map iteration order is not stable.
	JobOrder []string `json:"job_order"`

	// Deviations records every place this platform knowingly differs from GHA
	// for this workflow, so the UI can surface it at the point it matters.
	Deviations []Deviation `json:"deviations,omitempty"`
}

// Deviation is a documented, surfaced difference from GitHub Actions.
type Deviation struct {
	// Path is a YAML path like "jobs.build.steps[2].shell".
	Path string `json:"path"`
	// What GHA does.
	GHABehavior string `json:"gha_behavior"`
	// What this platform does instead.
	OurBehavior string `json:"our_behavior"`
	// Why, in one sentence.
	Rationale string `json:"rationale"`
}

// Triggers is the parsed `on:` block.
type Triggers struct {
	Push             *BranchFilter        `json:"push,omitempty"`
	PullRequest      *PullRequestFilter   `json:"pull_request,omitempty"`
	WorkflowDispatch *WorkflowDispatch    `json:"workflow_dispatch,omitempty"`
	Schedule         []ScheduleTrigger    `json:"schedule,omitempty"`
	WorkflowCall     *WorkflowCall        `json:"workflow_call,omitempty"`
	Other            map[string]RawEvents `json:"other,omitempty"`
}

// RawEvents holds an event this platform accepts but filters only by name.
type RawEvents struct {
	Types []string `json:"types,omitempty"`
}

// BranchFilter is the branches/tags/paths filter shared by push and PR events.
type BranchFilter struct {
	Branches       []string `json:"branches,omitempty"`
	BranchesIgnore []string `json:"branches_ignore,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	TagsIgnore     []string `json:"tags_ignore,omitempty"`
	Paths          []string `json:"paths,omitempty"`
	PathsIgnore    []string `json:"paths_ignore,omitempty"`
}

// PullRequestFilter adds activity types on top of the branch filter.
type PullRequestFilter struct {
	BranchFilter
	Types []string `json:"types,omitempty"`
}

// WorkflowDispatch declares typed manual inputs.
type WorkflowDispatch struct {
	Inputs map[string]*DispatchInput `json:"inputs,omitempty"`
	Order  []string                  `json:"order,omitempty"`
}

// DispatchInput is one workflow_dispatch input declaration.
type DispatchInput struct {
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Default     string   `json:"default,omitempty"`
	Type        string   `json:"type,omitempty"` // string | boolean | choice | number | environment
	Options     []string `json:"options,omitempty"`
}

// ScheduleTrigger is one cron entry.
type ScheduleTrigger struct {
	Cron string `json:"cron"`
}

// WorkflowCall declares a reusable workflow's interface.
type WorkflowCall struct {
	Inputs  map[string]*DispatchInput `json:"inputs,omitempty"`
	Secrets map[string]*CallSecret    `json:"secrets,omitempty"`
	Outputs map[string]*CallOutput    `json:"outputs,omitempty"`
}

// CallSecret is one declared secret of a reusable workflow.
type CallSecret struct {
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// CallOutput is one declared output of a reusable workflow.
type CallOutput struct {
	Description string `json:"description,omitempty"`
	Value       Expr   `json:"value"`
}

// Defaults is the defaults.run block.
type Defaults struct {
	Shell            string `json:"shell,omitempty"`
	WorkingDirectory string `json:"working_directory,omitempty"`
}

// Concurrency is a concurrency group declaration.
type Concurrency struct {
	Group            Expr `json:"group"`
	CancelInProgress Expr `json:"cancel_in_progress,omitempty"`
}

// Permissions is the GITHUB_TOKEN permission set for a job.
type Permissions struct {
	// All, when non-empty, is the shorthand form (`permissions: read-all`).
	All string `json:"all,omitempty"`
	// Scopes maps scope name to "read" | "write" | "none".
	Scopes map[string]string `json:"scopes,omitempty"`
}

// JobIR is one job as declared, before matrix expansion.
type JobIR struct {
	Key             string                    `json:"key"`
	Name            Expr                      `json:"name,omitempty"`
	Needs           []string                  `json:"needs,omitempty"`
	If              Expr                      `json:"if,omitempty"`
	RunsOn          RunsOn                    `json:"runs_on"`
	Env             map[string]Expr           `json:"env,omitempty"`
	Defaults        Defaults                  `json:"defaults,omitempty"`
	Outputs         map[string]Expr           `json:"outputs,omitempty"`
	TimeoutMinutes  Expr                      `json:"timeout_minutes,omitempty"`
	ContinueOnError Expr                      `json:"continue_on_error,omitempty"`
	Permissions     *Permissions              `json:"permissions,omitempty"`
	Concurrency     *Concurrency              `json:"concurrency,omitempty"`
	Strategy        *Strategy                 `json:"strategy,omitempty"`
	Environment     *Environment              `json:"environment,omitempty"`
	Steps           []*StepIR                 `json:"steps,omitempty"`
	Container       *ContainerSpec            `json:"container,omitempty"`
	Services        map[string]*ContainerSpec `json:"services,omitempty"`

	// Uses is set for a reusable-workflow call job; Steps is then empty.
	Uses    string          `json:"uses,omitempty"`
	With    map[string]Expr `json:"with,omitempty"`
	Secrets *JobSecrets     `json:"secrets,omitempty"`

	// Retry is this platform's first-class retry policy. Absent means the
	// default policy: infra retries, user failures never do.
	Retry *RetryPolicy `json:"retry,omitempty"`
}

// JobSecrets carries `secrets:` on a reusable-workflow call.
type JobSecrets struct {
	Inherit bool            `json:"inherit,omitempty"`
	Values  map[string]Expr `json:"values,omitempty"`
}

// RunsOn is the runner selector: labels, or a group, or both.
type RunsOn struct {
	Labels []Expr `json:"labels,omitempty"`
	Group  Expr   `json:"group,omitempty"`
}

// Environment is a deployment environment reference.
type Environment struct {
	Name Expr `json:"name"`
	URL  Expr `json:"url,omitempty"`
}

// ContainerSpec is a job container or service container.
type ContainerSpec struct {
	Image       Expr            `json:"image"`
	Credentials map[string]Expr `json:"credentials,omitempty"`
	Env         map[string]Expr `json:"env,omitempty"`
	Ports       []Expr          `json:"ports,omitempty"`
	Volumes     []Expr          `json:"volumes,omitempty"`
	Options     Expr            `json:"options,omitempty"`
}

// Strategy is the matrix strategy.
type Strategy struct {
	Matrix *Matrix `json:"matrix,omitempty"`
	// FailFast defaults to true when absent, matching GHA.
	FailFast    *bool `json:"fail_fast,omitempty"`
	MaxParallel Expr  `json:"max_parallel,omitempty"`
}

// Matrix is the matrix declaration. Dimensions preserves key order so that leg
// names are deterministic.
type Matrix struct {
	Dimensions map[string][]any `json:"dimensions,omitempty"`
	Order      []string         `json:"order,omitempty"`
	Include    []map[string]any `json:"include,omitempty"`
	Exclude    []map[string]any `json:"exclude,omitempty"`
	// FromExpr is set when the whole matrix is a ${{ fromJSON(...) }}; it is
	// resolved at plan time once needs outputs exist.
	FromExpr Expr `json:"from_expr,omitempty"`
}

// StepIR is one step as declared.
type StepIR struct {
	Number           int             `json:"number"`
	ID               string          `json:"id,omitempty"`
	Name             Expr            `json:"name,omitempty"`
	If               Expr            `json:"if,omitempty"`
	Uses             string          `json:"uses,omitempty"`
	Run              Expr            `json:"run,omitempty"`
	With             map[string]Expr `json:"with,omitempty"`
	Env              map[string]Expr `json:"env,omitempty"`
	Shell            string          `json:"shell,omitempty"`
	WorkingDirectory Expr            `json:"working_directory,omitempty"`
	ContinueOnError  Expr            `json:"continue_on_error,omitempty"`
	TimeoutMinutes   Expr            `json:"timeout_minutes,omitempty"`
	Retry            *RetryPolicy    `json:"retry,omitempty"`
}

// BackoffKind selects the retry delay curve.
type BackoffKind string

const (
	BackoffNone        BackoffKind = "none"
	BackoffFixed       BackoffKind = "fixed"
	BackoffLinear      BackoffKind = "linear"
	BackoffExponential BackoffKind = "exponential"
)

// RetryPolicy is declarative and first-class, per step and per job.
type RetryPolicy struct {
	Attempts int            `json:"attempts"`
	On       []FailureClass `json:"on"`
	Backoff  BackoffKind    `json:"backoff"`
	Initial  time.Duration  `json:"initial"`
	Max      time.Duration  `json:"max"`
	Jitter   bool           `json:"jitter"`
}

// DefaultRetryPolicy is what applies when a workflow declares nothing: infra
// failures retry three times with exponential backoff, user failures never
// retry, config errors never retry because retrying cannot fix them.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		Attempts: 3,
		On:       []FailureClass{ClassInfra},
		Backoff:  BackoffExponential,
		Initial:  5 * time.Second,
		Max:      2 * time.Minute,
		Jitter:   true,
	}
}

// Retries reports whether this policy retries the given class at the given
// attempt number (1-based, so attempt 1 is the first try).
func (p RetryPolicy) Retries(class FailureClass, attempt int) bool {
	if attempt >= p.Attempts {
		return false
	}
	for _, c := range p.On {
		if c == class {
			return true
		}
	}
	return false
}

// Delay is the wait before the given attempt number (1-based: the delay before
// attempt 2 is Delay(2)).
func (p RetryPolicy) Delay(attempt int) time.Duration {
	if attempt < 2 {
		return 0
	}
	n := attempt - 1
	var d time.Duration
	switch p.Backoff {
	case BackoffNone:
		return 0
	case BackoffFixed:
		d = p.Initial
	case BackoffLinear:
		d = p.Initial * time.Duration(n)
	default:
		d = p.Initial
		for i := 1; i < n; i++ {
			d *= 2
			if p.Max > 0 && d >= p.Max {
				break
			}
		}
	}
	if p.Max > 0 && d > p.Max {
		d = p.Max
	}
	return d
}
