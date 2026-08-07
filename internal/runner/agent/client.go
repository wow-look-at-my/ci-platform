// Package agent is the runner agent: it registers with the control plane,
// claims one job at a time, builds a sandbox for it, executes its steps, and
// reports the result.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/classify"
	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
)

// ControlPlane is everything the agent needs from the control plane. The agent
// is written against the interface so its loop is testable without a server.
type ControlPlane interface {
	Register(ctx context.Context, req protocol.RegisterRequest) (protocol.RegisterResponse, error)
	Acquire(ctx context.Context, req protocol.AcquireRequest) (protocol.AcquireResponse, error)
	Heartbeat(ctx context.Context, req protocol.HeartbeatRequest) (protocol.HeartbeatResponse, error)
	Setup(ctx context.Context, req protocol.SetupRequest) error
	Logs(ctx context.Context, req protocol.LogBatch) error
	StepStart(ctx context.Context, req protocol.StepStartRequest) error
	StepEnd(ctx context.Context, req protocol.StepEndRequest) error
	Annotate(ctx context.Context, req protocol.AnnotateRequest) error
	Complete(ctx context.Context, req protocol.CompleteRequest) (protocol.CompleteResponse, error)
	Release(ctx context.Context, req protocol.ReleaseRequest) error
}

// Error is a failed control-plane call. It carries its own classification: the
// control plane being unreachable is the platform's failure, never the
// workflow's, and no caller has to re-derive that.
type Error struct {
	Path     string
	Status   int
	Attempts int
	Err      error
	Decision classify.Decision
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("control plane %s: HTTP %d after %d attempt(s): %v", e.Path, e.Status, e.Attempts, e.Err)
	}
	return fmt.Sprintf("control plane %s: after %d attempt(s): %v", e.Path, e.Attempts, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Class is the failure class of this error, always infra.
func (e *Error) Class() model.FailureClass { return e.Decision.Class }

// ClientConfig configures the HTTP client.
type ClientConfig struct {
	// BaseURL is the control plane root, e.g. https://ci.example.com.
	BaseURL string
	// Token authenticates the runner.
	Token string
	HTTP  *http.Client
	// MaxAttempts bounds retries per call. Zero means the default.
	MaxAttempts int
	Backoff     time.Duration
	MaxBackoff  time.Duration
	// Sleep is injectable so tests do not wait out real backoff.
	Sleep func(context.Context, time.Duration) error
}

// Client speaks protocol over HTTP+JSON, retrying transient failures.
type Client struct {
	cfg        ClientConfig
	classifier classify.Classifier
}

// NewClient validates the configuration and returns a client. A missing base
// URL or token is a hard error: there is no unauthenticated mode.
func NewClient(cfg ClientConfig) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("agent: control plane URL is required")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("agent: runner token is required")
	}
	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 0}
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = 500 * time.Millisecond
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 15 * time.Second
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepCtx
	}
	return &Client{cfg: cfg}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) Register(ctx context.Context, req protocol.RegisterRequest) (protocol.RegisterResponse, error) {
	var out protocol.RegisterResponse
	err := c.post(ctx, protocol.PathRegister, req, &out)
	return out, err
}

func (c *Client) Acquire(ctx context.Context, req protocol.AcquireRequest) (protocol.AcquireResponse, error) {
	var out protocol.AcquireResponse
	err := c.post(ctx, protocol.PathAcquire, req, &out)
	return out, err
}

func (c *Client) Heartbeat(ctx context.Context, req protocol.HeartbeatRequest) (protocol.HeartbeatResponse, error) {
	var out protocol.HeartbeatResponse
	err := c.post(ctx, protocol.PathHeartbeat, req, &out)
	return out, err
}

func (c *Client) Setup(ctx context.Context, req protocol.SetupRequest) error {
	return c.post(ctx, protocol.PathSetup, req, nil)
}

func (c *Client) Logs(ctx context.Context, req protocol.LogBatch) error {
	return c.post(ctx, protocol.PathLogs, req, nil)
}

func (c *Client) StepStart(ctx context.Context, req protocol.StepStartRequest) error {
	return c.post(ctx, protocol.PathStepStart, req, nil)
}

func (c *Client) StepEnd(ctx context.Context, req protocol.StepEndRequest) error {
	return c.post(ctx, protocol.PathStepEnd, req, nil)
}

func (c *Client) Annotate(ctx context.Context, req protocol.AnnotateRequest) error {
	return c.post(ctx, protocol.PathAnnotate, req, nil)
}

func (c *Client) Complete(ctx context.Context, req protocol.CompleteRequest) (protocol.CompleteResponse, error) {
	var out protocol.CompleteResponse
	err := c.post(ctx, protocol.PathComplete, req, &out)
	return out, err
}

func (c *Client) Release(ctx context.Context, req protocol.ReleaseRequest) error {
	return c.post(ctx, protocol.PathRelease, req, nil)
}

// post sends one request, retrying network failures and 5xx/429 with
// exponential backoff. A 4xx is not retried: the control plane rejected the
// request and repeating it cannot help.
func (c *Client) post(ctx context.Context, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return &Error{Path: path, Attempts: 0, Err: err, Decision: c.classifyErr(path, 0, err)}
	}

	var lastErr error
	var lastStatus int
	for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
		status, respBody, err := c.once(ctx, path, payload)
		switch {
		case err == nil && status/100 == 2:
			if out != nil && len(respBody) > 0 {
				if err := json.Unmarshal(respBody, out); err != nil {
					return &Error{Path: path, Status: status, Attempts: attempt, Err: err,
						Decision: c.classifyErr(path, status, err)}
				}
			}
			return nil
		case err == nil:
			lastStatus = status
			lastErr = fmt.Errorf("HTTP %d: %s", status, strings.TrimSpace(string(respBody)))
			if status/100 != 5 && status != http.StatusTooManyRequests {
				return &Error{Path: path, Status: status, Attempts: attempt, Err: lastErr,
					Decision: c.classifyErr(path, status, lastErr)}
			}
		default:
			if ctx.Err() != nil {
				return &Error{Path: path, Attempts: attempt, Err: err, Decision: c.classifyErr(path, 0, err)}
			}
			lastErr = err
			lastStatus = 0
		}

		if attempt == c.cfg.MaxAttempts {
			break
		}
		if err := c.cfg.Sleep(ctx, backoff(c.cfg.Backoff, c.cfg.MaxBackoff, attempt)); err != nil {
			return &Error{Path: path, Status: lastStatus, Attempts: attempt, Err: errors.Join(lastErr, err),
				Decision: c.classifyErr(path, lastStatus, lastErr)}
		}
	}
	return &Error{Path: path, Status: lastStatus, Attempts: c.cfg.MaxAttempts, Err: lastErr,
		Decision: c.classifyErr(path, lastStatus, lastErr)}
}

func (c *Client) once(ctx context.Context, path string, payload []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("X-CI-Api-Version", protocol.APIVersion)

	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// classifyErr records why a control-plane failure is the platform's. The phase
// is "dispatch", which classify treats as a platform phase, so the decision is
// infra even when the message matches no rule.
func (c *Client) classifyErr(path string, status int, err error) classify.Decision {
	return c.classifier.Classify(classify.Signal{
		Err:        err,
		HTTPStatus: status,
		Phase:      "dispatch",
		Host:       c.cfg.BaseURL + path,
	})
}

func backoff(base, max time.Duration, attempt int) time.Duration {
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	if d > max {
		return max
	}
	return d
}
