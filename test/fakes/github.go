package fakes

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// CheckRun is what the platform reported to the Checks API, as the fake saw it.
type CheckRun struct {
	ID          int64          `json:"id"`
	Name        string         `json:"name"`
	HeadSHA     string         `json:"head_sha"`
	Status      string         `json:"status"`
	Conclusion  string         `json:"conclusion"`
	DetailsURL  string         `json:"details_url"`
	ExternalID  string         `json:"external_id"`
	StartedAt   string         `json:"started_at"`
	CompletedAt string         `json:"completed_at"`
	Output      CheckOutput    `json:"output"`
	Actions     []CheckAction  `json:"actions"`
	Raw         map[string]any `json:"-"`
	// Updates counts how many API calls touched this check run, which is how a
	// test asserts that updates were coalesced rather than sent per step.
	Updates int `json:"-"`
}

// CheckOutput is the check run's output block.
type CheckOutput struct {
	Title       string           `json:"title"`
	Summary     string           `json:"summary"`
	Text        string           `json:"text"`
	Annotations []map[string]any `json:"annotations"`
}

// CheckAction is one button offered on a check run.
type CheckAction struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Identifier  string `json:"identifier"`
}

// CommitStatus is one legacy commit status the platform posted.
type CommitStatus struct {
	SHA         string `json:"-"`
	State       string `json:"state"`
	Context     string `json:"context"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
}

// GitHub is a fake GitHub API. It records everything the platform reported so a
// test can assert on the status path end to end, and it can be made to fail so
// the platform's own retry behaviour is exercised.
type GitHub struct {
	srv *httptest.Server

	mu             sync.Mutex
	checkRuns      map[int64]*CheckRun
	checkRunOrder  []int64
	statuses       []CommitStatus
	nextID         int64
	files          map[string]string
	failNext       int
	failStatusCode int
	requests       int

	// WebhookSecret is used by SignWebhook.
	WebhookSecret string
}

// NewGitHub starts a fake GitHub API server.
func NewGitHub() *GitHub {
	g := &GitHub{
		checkRuns:     map[int64]*CheckRun{},
		files:         map[string]string{},
		nextID:        1000,
		WebhookSecret: "test-webhook-secret",
	}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	return g
}

// URL is the API base URL to configure the platform with.
func (g *GitHub) URL() string { return g.srv.URL }

// Close shuts the server down.
func (g *GitHub) Close() { g.srv.Close() }

// AddFile makes a repo file readable at every ref, so the platform can discover
// workflow files. Path is repo-relative.
func (g *GitHub) AddFile(path, content string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.files[path] = content
}

// FailNext makes the next n API calls return the given status, so a test can
// prove the platform retries rather than dropping a status update.
func (g *GitHub) FailNext(n, status int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failNext = n
	g.failStatusCode = status
}

// CheckRuns returns the recorded check runs in creation order.
func (g *GitHub) CheckRuns() []CheckRun {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]CheckRun, 0, len(g.checkRunOrder))
	for _, id := range g.checkRunOrder {
		out = append(out, *g.checkRuns[id])
	}
	return out
}

// CheckRunNamed returns the recorded check run with the given name.
func (g *GitHub) CheckRunNamed(name string) (CheckRun, bool) {
	for _, cr := range g.CheckRuns() {
		if cr.Name == name {
			return cr, true
		}
	}
	return CheckRun{}, false
}

// Statuses returns the recorded legacy commit statuses in order.
func (g *GitHub) Statuses() []CommitStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]CommitStatus(nil), g.statuses...)
}

// Requests is the total number of API calls received.
func (g *GitHub) Requests() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requests
}

// SignWebhook produces the X-Hub-Signature-256 header value for a body.
func (g *GitHub) SignWebhook(body []byte) string {
	mac := hmac.New(sha256.New, []byte(g.WebhookSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifyWebhookSignature is the check a receiver should make; it exists here so
// a test can assert the fake and the real verifier agree.
func (g *GitHub) VerifyWebhookSignature(body []byte, header string) bool {
	return hmac.Equal([]byte(g.SignWebhook(body)), []byte(header))
}

func (g *GitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.requests++
	if g.failNext > 0 {
		g.failNext--
		code := g.failStatusCode
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"message":"injected failure %d"}`, code)
		return
	}
	g.mu.Unlock()

	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/access_tokens") && r.Method == http.MethodPost:
		g.writeJSON(w, http.StatusCreated, map[string]any{
			"token":      "ghs_faketoken",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})

	case strings.HasSuffix(path, "/check-runs") && r.Method == http.MethodPost:
		g.createCheckRun(w, r)

	case strings.Contains(path, "/check-runs/") && r.Method == http.MethodPatch:
		g.updateCheckRun(w, r, path)

	case strings.Contains(path, "/statuses/") && r.Method == http.MethodPost:
		g.createStatus(w, r, path)

	case strings.Contains(path, "/contents/"):
		g.serveContents(w, path)

	default:
		g.writeJSON(w, http.StatusOK, map[string]any{})
	}
}

func (g *GitHub) createCheckRun(w http.ResponseWriter, r *http.Request) {
	body := decode(r)
	g.mu.Lock()
	g.nextID++
	id := g.nextID
	cr := &CheckRun{ID: id, Updates: 1, Raw: body}
	applyCheckRun(cr, body)
	g.checkRuns[id] = cr
	g.checkRunOrder = append(g.checkRunOrder, id)
	g.mu.Unlock()

	g.writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": cr.Name})
}

func (g *GitHub) updateCheckRun(w http.ResponseWriter, r *http.Request, path string) {
	idStr := path[strings.LastIndex(path, "/")+1:]
	var id int64
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
		g.writeJSON(w, http.StatusBadRequest, map[string]any{"message": "bad check run id"})
		return
	}
	body := decode(r)

	g.mu.Lock()
	cr, ok := g.checkRuns[id]
	if !ok {
		g.mu.Unlock()
		g.writeJSON(w, http.StatusNotFound, map[string]any{"message": "no such check run"})
		return
	}
	cr.Updates++
	applyCheckRun(cr, body)
	g.mu.Unlock()

	g.writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (g *GitHub) createStatus(w http.ResponseWriter, r *http.Request, path string) {
	sha := path[strings.LastIndex(path, "/")+1:]
	body := decode(r)

	g.mu.Lock()
	g.statuses = append(g.statuses, CommitStatus{
		SHA:         sha,
		State:       str(body["state"]),
		Context:     str(body["context"]),
		Description: str(body["description"]),
		TargetURL:   str(body["target_url"]),
	})
	g.mu.Unlock()

	g.writeJSON(w, http.StatusCreated, map[string]any{"state": str(body["state"])})
}

func (g *GitHub) serveContents(w http.ResponseWriter, path string) {
	repoPath := path[strings.Index(path, "/contents/")+len("/contents/"):]
	g.mu.Lock()
	content, ok := g.files[repoPath]
	var dir []map[string]any
	if !ok {
		// A directory listing, so workflow discovery finds the files.
		prefix := strings.TrimSuffix(repoPath, "/") + "/"
		for p := range g.files {
			if strings.HasPrefix(p, prefix) && !strings.Contains(strings.TrimPrefix(p, prefix), "/") {
				dir = append(dir, map[string]any{"name": strings.TrimPrefix(p, prefix), "path": p, "type": "file"})
			}
		}
	}
	g.mu.Unlock()

	if ok {
		g.writeJSON(w, http.StatusOK, map[string]any{
			"type": "file", "path": repoPath, "encoding": "base64",
			"content": encodeBase64(content),
		})
		return
	}
	if len(dir) > 0 {
		g.writeJSON(w, http.StatusOK, dir)
		return
	}
	g.writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
}

func applyCheckRun(cr *CheckRun, body map[string]any) {
	if v := str(body["name"]); v != "" {
		cr.Name = v
	}
	if v := str(body["head_sha"]); v != "" {
		cr.HeadSHA = v
	}
	if v := str(body["status"]); v != "" {
		cr.Status = v
	}
	if v := str(body["conclusion"]); v != "" {
		cr.Conclusion = v
	}
	if v := str(body["details_url"]); v != "" {
		cr.DetailsURL = v
	}
	if v := str(body["external_id"]); v != "" {
		cr.ExternalID = v
	}
	if v := str(body["started_at"]); v != "" {
		cr.StartedAt = v
	}
	if v := str(body["completed_at"]); v != "" {
		cr.CompletedAt = v
	}
	if out, ok := body["output"].(map[string]any); ok {
		cr.Output.Title = str(out["title"])
		cr.Output.Summary = str(out["summary"])
		if t := str(out["text"]); t != "" {
			cr.Output.Text = t
		}
		if as, ok := out["annotations"].([]any); ok {
			for _, a := range as {
				if m, ok := a.(map[string]any); ok {
					cr.Output.Annotations = append(cr.Output.Annotations, m)
				}
			}
		}
	}
	if as, ok := body["actions"].([]any); ok {
		cr.Actions = nil
		for _, a := range as {
			if m, ok := a.(map[string]any); ok {
				cr.Actions = append(cr.Actions, CheckAction{
					Label:       str(m["label"]),
					Description: str(m["description"]),
					Identifier:  str(m["identifier"]),
				})
			}
		}
	}
}

func (g *GitHub) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-RateLimit-Remaining", "4999")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request) map[string]any {
	var m map[string]any
	_ = json.NewDecoder(r.Body).Decode(&m)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
