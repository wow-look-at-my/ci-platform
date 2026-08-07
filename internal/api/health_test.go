package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// staticHealth reports a fixed set of subsystems.
type staticHealth struct{ subs []Subsystem }

func (h staticHealth) Subsystems(context.Context) []Subsystem { return h.subs }

func TestHealthzOK(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Blobs = &fakeBlobs{} })
	w := h.do(t, "GET", "/healthz", "")
	require.Equal(t, http.StatusOK, w.Code)
	got := decode[HealthDTO](t, w)
	assert.Equal(t, StateOK, got.Status)
	assert.True(t, got.StoreDurable)
	names := map[string]Subsystem{}
	for _, s := range got.Subsystems {
		names[s.Name] = s
	}
	assert.Equal(t, StateOK, names["store"].State)
	assert.Equal(t, StateOK, names["scheduler"].State)
	assert.Equal(t, StateOK, names["logs"].State)
	assert.Equal(t, StateOK, names["artifact_blobs"].State)
}

func TestHealthzNamesEveryDegradation(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Logs = nil
		c.Health = staticHealth{subs: []Subsystem{{Name: "webhooks", State: StateDegraded, Detail: "delivery backlog is 400 events deep"}}}
	})
	h.st.durable = false

	w := h.do(t, "GET", "/healthz", "")
	require.Equal(t, http.StatusOK, w.Code, "degraded is still serving, so /healthz stays 2xx")
	got := decode[HealthDTO](t, w)
	assert.Equal(t, StateDegraded, got.Status)
	assert.False(t, got.StoreDurable)

	byName := map[string]Subsystem{}
	for _, s := range got.Subsystems {
		byName[s.Name] = s
	}
	assert.Equal(t, StateDegraded, byName["store"].State)
	assert.Contains(t, byName["store"].Detail, "not durable")
	assert.Equal(t, StateDegraded, byName["logs"].State)
	assert.Contains(t, byName["logs"].Detail, "job logs are unavailable")
	assert.Equal(t, StateDegraded, byName["artifact_blobs"].State)
	assert.Equal(t, StateDegraded, byName["webhooks"].State)
	assert.Contains(t, byName["webhooks"].Detail, "400 events deep")
}

func TestHealthzIs503WhenTheStoreIsDown(t *testing.T) {
	h := newHarness(t)
	h.st.listErr = errors.New("connection refused")
	w := h.do(t, "GET", "/healthz", "")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	got := decode[HealthDTO](t, w)
	assert.Equal(t, StateDown, got.Status)
	assert.Contains(t, got.Subsystems[0].Detail, "connection refused")
}

// The updater endpoint is a status-code-only contract: a non-2xx answer makes
// the orchestrator roll back a running release. A degraded control plane is
// still a serving control plane, so it must stay 2xx and let /healthz carry the
// degradation.
func TestUpdaterHealthStays2xxWhileDegraded(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Logs = nil
		c.Controller = nil
		c.Health = staticHealth{subs: []Subsystem{{Name: "webhooks", State: StateDown, Detail: "webhook receiver is not accepting deliveries"}}}
	})
	h.st.durable = false
	h.st.listErr = errors.New("connection refused")

	w := h.do(t, "GET", "/.well-known/docker-updater/health", "")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, w.Code >= 200 && w.Code < 300, "the updater endpoint must never answer non-2xx for a degraded subsystem")

	// The same instance reports the degradation where an operator reads it.
	w = h.do(t, "GET", "/healthz", "")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	got := decode[HealthDTO](t, w)
	assert.Equal(t, StateDown, got.Status)
	details := ""
	for _, s := range got.Subsystems {
		details += s.Name + "=" + string(s.State) + " " + s.Detail + "\n"
	}
	assert.Contains(t, details, "store=down")
	assert.Contains(t, details, "scheduler=degraded")
	assert.Contains(t, details, "logs=degraded")
	assert.Contains(t, details, "webhooks=down")
}

func TestRollup(t *testing.T) {
	assert.Equal(t, StateOK, rollup([]Subsystem{{State: StateOK}}))
	assert.Equal(t, StateDegraded, rollup([]Subsystem{{State: StateOK}, {State: StateDegraded}}))
	assert.Equal(t, StateDown, rollup([]Subsystem{{State: StateDegraded}, {State: StateDown}}))
	assert.Equal(t, StateOK, rollup(nil))
}
