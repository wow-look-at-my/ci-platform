package webhook

import (
	"encoding/json"
	"time"
)

// Meta is what every delivery carries. Raw is kept because a re-run has to
// rebuild the `github` expression context from the original payload rather than
// re-fetching it.
type Meta struct {
	// DeliveryID is X-GitHub-Delivery, the dedupe key for redeliveries.
	DeliveryID string          `json:"delivery_id"`
	Event      string          `json:"event"`
	Action     string          `json:"action,omitempty"`
	Raw        json.RawMessage `json:"-"`
	ReceivedAt time.Time       `json:"received_at"`

	InstallationID int64      `json:"installation_id,omitempty"`
	Repo           Repository `json:"repository"`
	Sender         User       `json:"sender"`
}

// User is a GitHub account reference.
type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

// Repository is the repo fields the platform needs off a payload.
type Repository struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	Fork          bool   `json:"fork"`
	DefaultBranch string `json:"default_branch"`
	Owner         User   `json:"owner"`
}

// installationRef appears on every installation-delivered payload.
type installationRef struct {
	ID int64 `json:"id"`
}

// envelope is the subset of every payload that fills Meta.
type envelope struct {
	Action       string          `json:"action"`
	Repository   Repository      `json:"repository"`
	Sender       User            `json:"sender"`
	Installation installationRef `json:"installation"`
}

// Commit is the head commit of a push.
type Commit struct {
	ID        string    `json:"id"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
	Author    struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Username string `json:"username"`
	} `json:"author"`
}

// PushEvent is a push to a ref.
type PushEvent struct {
	Meta
	Ref        string  `json:"ref"`
	Before     string  `json:"before"`
	After      string  `json:"after"`
	Created    bool    `json:"created"`
	Deleted    bool    `json:"deleted"`
	Forced     bool    `json:"forced"`
	HeadCommit *Commit `json:"head_commit"`
}

// Branch returns the branch name for a branch push, "" for a tag or anything
// else.
func (e *PushEvent) Branch() string { return trimPrefix(e.Ref, "refs/heads/") }

// Tag returns the tag name for a tag push, "" otherwise.
func (e *PushEvent) Tag() string { return trimPrefix(e.Ref, "refs/tags/") }

// PRRef is one side of a pull request.
type PRRef struct {
	Label string     `json:"label"`
	Ref   string     `json:"ref"`
	SHA   string     `json:"sha"`
	Repo  Repository `json:"repo"`
}

// PullRequest is the subset of a PR the scheduler needs.
type PullRequest struct {
	ID     int64  `json:"id"`
	Number int    `json:"number"`
	State  string `json:"state"`
	Title  string `json:"title"`
	Draft  bool   `json:"draft"`
	Merged bool   `json:"merged"`
	User   User   `json:"user"`
	Head   PRRef  `json:"head"`
	Base   PRRef  `json:"base"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// PullRequestEvent is any pull_request action.
type PullRequestEvent struct {
	Meta
	Number      int         `json:"number"`
	PullRequest PullRequest `json:"pull_request"`
}

// IsFork reports whether the head is on a different repository than the base.
// A fork PR gates secrets, OIDC, and the approval requirement, so this is
// decided from the payload rather than inferred later.
func (e *PullRequestEvent) IsFork() bool {
	head, base := e.PullRequest.Head.Repo, e.PullRequest.Base.Repo
	if head.ID != 0 && base.ID != 0 {
		return head.ID != base.ID
	}
	return head.FullName != base.FullName
}

// WorkflowDispatchEvent is a manual run.
type WorkflowDispatchEvent struct {
	Meta
	Ref      string         `json:"ref"`
	Workflow string         `json:"workflow"`
	Inputs   map[string]any `json:"inputs"`
}

// CheckSuiteRef is the suite a check run belongs to.
type CheckSuiteRef struct {
	ID         int64  `json:"id"`
	HeadSHA    string `json:"head_sha"`
	HeadBranch string `json:"head_branch"`
}

// CheckRun is the check run a re-run or action request names. ExternalID is the
// platform's own job identity, which is how a button press maps back to a job.
type CheckRun struct {
	ID         int64         `json:"id"`
	Name       string        `json:"name"`
	HeadSHA    string        `json:"head_sha"`
	ExternalID string        `json:"external_id"`
	Status     string        `json:"status"`
	Conclusion string        `json:"conclusion"`
	DetailsURL string        `json:"details_url"`
	CheckSuite CheckSuiteRef `json:"check_suite"`
}

// RequestedAction names the actions[] button that was pressed.
type RequestedAction struct {
	Identifier string `json:"identifier"`
}

// CheckRunEvent covers both "rerequested" and "requested_action".
type CheckRunEvent struct {
	Meta
	CheckRun        CheckRun         `json:"check_run"`
	RequestedAction *RequestedAction `json:"requested_action,omitempty"`
}

// CheckSuite is a suite of check runs against one SHA.
type CheckSuite struct {
	ID         int64  `json:"id"`
	HeadSHA    string `json:"head_sha"`
	HeadBranch string `json:"head_branch"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// CheckSuiteEvent is a suite-level re-run request.
type CheckSuiteEvent struct {
	Meta
	CheckSuite CheckSuite `json:"check_suite"`
}

// InstallationAccount mirrors the account an App is installed on.
type InstallationAccount struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

// Installation is the installation carried by installation events.
type Installation struct {
	ID                  int64               `json:"id"`
	Account             InstallationAccount `json:"account"`
	RepositorySelection string              `json:"repository_selection"`
	TargetType          string              `json:"target_type"`
}

// InstallationEvent covers installation and installation_repositories. Added
// and Removed are only populated by installation_repositories.
type InstallationEvent struct {
	Meta
	Installation Installation `json:"installation"`
	Repositories []Repository `json:"repositories"`
	Added        []Repository `json:"repositories_added"`
	Removed      []Repository `json:"repositories_removed"`
}

func trimPrefix(s, p string) string {
	if len(s) > len(p) && s[:len(p)] == p {
		return s[len(p):]
	}
	return ""
}
