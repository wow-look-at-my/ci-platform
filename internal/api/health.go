package api

import (
	"context"
	"net/http"
	"time"
)

// HealthDTO is the /healthz body: one line per subsystem plus the rollup.
type HealthDTO struct {
	Status       SubsystemState `json:"status"`
	StoreDurable bool           `json:"store_durable"`
	Subsystems   []Subsystem    `json:"subsystems"`
	At           time.Time      `json:"at"`
}

// subsystems collects health from the store the API can reach itself and from
// whatever the lead wired in.
func (s *Server) subsystems(ctx context.Context) []Subsystem {
	out := []Subsystem{}

	durable := s.cfg.Store.Durable()
	storeSub := Subsystem{Name: "store", State: StateOK}
	if _, err := s.cfg.Store.ListRepos(ctx); err != nil {
		storeSub.State = StateDown
		storeSub.Detail = "store is unreachable: " + err.Error()
	} else if !durable {
		storeSub.State = StateDegraded
		storeSub.Detail = "store is not durable: a control-plane restart loses queued work"
	}
	out = append(out, storeSub)

	out = append(out,
		wiring("scheduler", s.cfg.Controller != nil, "no scheduler is wired in, so cancel and re-run are unavailable"),
		wiring("logs", s.cfg.Logs != nil, "no log source is wired in, so job logs are unavailable"),
		wiring("artifact_blobs", s.cfg.Blobs != nil, "no blob store is wired in, so artifact downloads are unavailable"),
	)

	if s.cfg.Health != nil {
		out = append(out, s.cfg.Health.Subsystems(ctx)...)
	}
	return out
}

// wiring reports a missing collaborator as degraded, naming what it costs.
func wiring(name string, present bool, missingDetail string) Subsystem {
	if present {
		return Subsystem{Name: name, State: StateOK}
	}
	return Subsystem{Name: name, State: StateDegraded, Detail: missingDetail}
}

// rollup is the worst state present.
func rollup(subs []Subsystem) SubsystemState {
	worst := StateOK
	for _, s := range subs {
		switch s.State {
		case StateDown:
			return StateDown
		case StateDegraded:
			worst = StateDegraded
		}
	}
	return worst
}

// healthz reports the truth: the body names every degraded subsystem, and a
// subsystem that is fully down makes this endpoint 503.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	subs := s.subsystems(r.Context())
	body := HealthDTO{Status: rollup(subs), StoreDurable: s.cfg.Store.Durable(), Subsystems: subs, At: s.now()}
	code := http.StatusOK
	if body.Status == StateDown {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, body)
}

// updaterHealth is a STATUS-CODE-ONLY contract: the orchestrator reads the code
// and ignores the body. It must stay 2xx while a subsystem is degraded, because
// answering non-2xx here is what makes the orchestrator roll back a running
// release -- which would replace a degraded-but-serving control plane with a
// redeploy that fixes nothing. Degradation is reported in /healthz's body; this
// endpoint answers only "this process is up and serving HTTP".
func (s *Server) updaterHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}
