// Package webhook verifies and dispatches GitHub webhook deliveries.
//
// Verification is mandatory: an empty secret is a configuration error, never a
// reason to skip the HMAC check. An event the platform handles that fails is
// answered 5xx so GitHub retries it; an event it does not handle is answered
// 202 and logged.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// SignatureHeader is the header Verify reads.
const SignatureHeader = "X-Hub-Signature-256"

// DeliveryHeader carries the id used to dedupe redeliveries.
const DeliveryHeader = "X-GitHub-Delivery"

// EventHeader carries the event type.
const EventHeader = "X-GitHub-Event"

// DefaultMaxBodyBytes bounds a delivery body. GitHub caps payloads at 25MB.
const DefaultMaxBodyBytes int64 = 25 << 20

var (
	// ErrNoSecret means the platform was started without a webhook secret.
	ErrNoSecret = errors.New("webhook: HMAC secret is empty; set the webhook secret (unverified deliveries are never accepted)")
	// ErrMissingSignature means the delivery carried no signature header.
	ErrMissingSignature = errors.New("webhook: " + SignatureHeader + " header is missing")
	// ErrMalformedSignature means the header was not "sha256=<hex>".
	ErrMalformedSignature = errors.New("webhook: " + SignatureHeader + " is not sha256=<hex>")
	// ErrBadSignature means the HMAC did not match.
	ErrBadSignature = errors.New("webhook: signature does not match the body")
)

// Verify checks the HMAC-SHA256 signature of a delivery body in constant time.
func Verify(secret string, header string, body []byte) error {
	if secret == "" {
		return ErrNoSecret
	}
	if header == "" {
		return ErrMissingSignature
	}
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return fmt.Errorf("%w: got %q", ErrMalformedSignature, header)
	}
	got, err := hex.DecodeString(header[len(prefix):])
	if err != nil || len(got) != sha256.Size {
		return fmt.Errorf("%w: signature is not %d hex bytes", ErrMalformedSignature, sha256.Size)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), got) {
		return ErrBadSignature
	}
	return nil
}

// Sign produces the header value for a body. Used by tests and by the
// self-check that proves a configured secret round-trips.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Sink receives parsed events. An error from any method is answered 5xx so
// GitHub redelivers.
type Sink interface {
	Push(ctx context.Context, e *PushEvent) error
	PullRequest(ctx context.Context, e *PullRequestEvent) error
	WorkflowDispatch(ctx context.Context, e *WorkflowDispatchEvent) error
	CheckRunRerequested(ctx context.Context, e *CheckRunEvent) error
	CheckSuiteRerequested(ctx context.Context, e *CheckSuiteEvent) error
	RequestedAction(ctx context.Context, e *CheckRunEvent) error
	Installation(ctx context.Context, e *InstallationEvent) error
}

// Deduper answers whether a delivery id has already been processed. It is
// optional; when unset the handler exposes Meta.DeliveryID and leaves the
// decision to the Sink.
type Deduper interface {
	// Seen records the delivery and reports whether it had been seen before.
	Seen(ctx context.Context, deliveryID string) (bool, error)
}

// Handler is the http.Handler for the webhook endpoint.
type Handler struct {
	secret       string
	sink         Sink
	log          *slog.Logger
	now          func() time.Time
	maxBodyBytes int64
	dedupe       Deduper
}

// Option configures a Handler.
type Option func(*Handler)

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(h *Handler) { h.log = l } }

// WithClock injects the clock.
func WithClock(now func() time.Time) Option { return func(h *Handler) { h.now = now } }

// WithMaxBody bounds the accepted body size.
func WithMaxBody(n int64) Option { return func(h *Handler) { h.maxBodyBytes = n } }

// WithDeduper enables duplicate-delivery suppression.
func WithDeduper(d Deduper) Option { return func(h *Handler) { h.dedupe = d } }

// NewHandler builds a Handler. A missing secret or sink is a startup error.
func NewHandler(secret string, sink Sink, opts ...Option) (*Handler, error) {
	if secret == "" {
		return nil, ErrNoSecret
	}
	if sink == nil {
		return nil, errors.New("webhook: sink is nil; there is nothing to deliver events to")
	}
	h := &Handler{
		secret:       secret,
		sink:         sink,
		log:          slog.Default(),
		now:          time.Now,
		maxBodyBytes: DefaultMaxBodyBytes,
	}
	for _, o := range opts {
		o(h)
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	if h.now == nil {
		h.now = time.Now
	}
	if h.maxBodyBytes <= 0 {
		h.maxBodyBytes = DefaultMaxBodyBytes
	}
	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "webhook: POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, h.maxBodyBytes+1))
	if err != nil {
		http.Error(w, "webhook: reading body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if int64(len(body)) > h.maxBodyBytes {
		http.Error(w, fmt.Sprintf("webhook: body exceeds %d bytes", h.maxBodyBytes), http.StatusRequestEntityTooLarge)
		return
	}
	if err := Verify(h.secret, r.Header.Get(SignatureHeader), body); err != nil {
		h.log.Warn("webhook signature rejected", "err", err,
			"delivery", r.Header.Get(DeliveryHeader), "event", r.Header.Get(EventHeader))
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	event := r.Header.Get(EventHeader)
	delivery := r.Header.Get(DeliveryHeader)
	if event == "" {
		http.Error(w, "webhook: "+EventHeader+" header is missing", http.StatusBadRequest)
		return
	}

	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		http.Error(w, "webhook: body is not JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	meta := Meta{
		DeliveryID:     delivery,
		Event:          event,
		Action:         env.Action,
		Raw:            json.RawMessage(body),
		ReceivedAt:     h.now(),
		InstallationID: env.Installation.ID,
		Repo:           env.Repository,
		Sender:         env.Sender,
	}

	if h.dedupe != nil && delivery != "" {
		seen, err := h.dedupe.Seen(r.Context(), delivery)
		if err != nil {
			// Not knowing whether this is a duplicate is a failure, not a
			// reason to process it twice or drop it.
			h.log.Error("webhook dedupe check failed", "delivery", delivery, "event", event, "err", err)
			http.Error(w, "webhook: dedupe check failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if seen {
			h.log.Info("duplicate delivery ignored", "delivery", delivery, "event", event)
			writeStatus(w, http.StatusOK, "duplicate delivery "+delivery)
			return
		}
	}

	handled, err := h.dispatch(r.Context(), meta, body)
	switch {
	case err != nil:
		h.log.Error("webhook handler failed", "event", event, "action", meta.Action,
			"delivery", delivery, "repo", meta.Repo.FullName, "err", err)
		http.Error(w, "webhook: handling "+event+": "+err.Error(), http.StatusInternalServerError)
	case !handled:
		h.log.Info("ignored event", "event", event, "action", meta.Action, "delivery", delivery)
		writeStatus(w, http.StatusAccepted, "ignored event: "+describe(event, meta.Action))
	default:
		writeStatus(w, http.StatusOK, "ok")
	}
}

func describe(event, action string) string {
	if action == "" {
		return event
	}
	return event + "." + action
}

func writeStatus(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, msg+"\n")
}

// dispatch parses and routes one delivery. handled is false for events this
// platform deliberately ignores.
func (h *Handler) dispatch(ctx context.Context, meta Meta, body []byte) (handled bool, err error) {
	decode := func(v any) error {
		if err := json.Unmarshal(body, v); err != nil {
			return fmt.Errorf("decoding %s payload: %w", meta.Event, err)
		}
		return nil
	}
	switch meta.Event {
	case "push":
		var e PushEvent
		if err := decode(&e); err != nil {
			return true, err
		}
		e.Meta = meta
		return true, h.sink.Push(ctx, &e)

	case "pull_request":
		var e PullRequestEvent
		if err := decode(&e); err != nil {
			return true, err
		}
		e.Meta = meta
		return true, h.sink.PullRequest(ctx, &e)

	case "workflow_dispatch":
		var e WorkflowDispatchEvent
		if err := decode(&e); err != nil {
			return true, err
		}
		e.Meta = meta
		return true, h.sink.WorkflowDispatch(ctx, &e)

	case "check_run":
		var e CheckRunEvent
		if err := decode(&e); err != nil {
			return true, err
		}
		e.Meta = meta
		switch meta.Action {
		case "rerequested":
			return true, h.sink.CheckRunRerequested(ctx, &e)
		case "requested_action":
			if e.RequestedAction == nil || e.RequestedAction.Identifier == "" {
				return true, errors.New("check_run.requested_action carried no requested_action.identifier")
			}
			return true, h.sink.RequestedAction(ctx, &e)
		}
		return false, nil

	case "check_suite":
		if meta.Action != "rerequested" {
			return false, nil
		}
		var e CheckSuiteEvent
		if err := decode(&e); err != nil {
			return true, err
		}
		e.Meta = meta
		return true, h.sink.CheckSuiteRerequested(ctx, &e)

	case "installation", "installation_repositories":
		var e InstallationEvent
		if err := decode(&e); err != nil {
			return true, err
		}
		e.Meta = meta
		return true, h.sink.Installation(ctx, &e)
	}
	return false, nil
}
