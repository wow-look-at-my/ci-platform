package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// RunnerHostDTO is one machine and where it sits in the approval flow.
type RunnerHostDTO struct {
	Fingerprint  string    `json:"fingerprint"`
	State        string    `json:"state"`
	Name         string    `json:"name"`
	OS           string    `json:"os,omitempty"`
	Arch         string    `json:"arch,omitempty"`
	Version      string    `json:"version,omitempty"`
	Labels       []string  `json:"labels"`
	EnrolledFrom string    `json:"enrolled_from,omitempty"`
	EnrolledAt   time.Time `json:"enrolled_at"`
	ApprovedBy   string    `json:"approved_by,omitempty"`
	ApprovedAt   time.Time `json:"approved_at,omitzero"`
	LastSeenAt   time.Time `json:"last_seen_at,omitzero"`
	Note         string    `json:"note,omitempty"`
}

// RunnerHostListDTO is the approval page. PendingCount is called out because a
// host waiting for approval is a runner that looks broken to whoever set it up.
type RunnerHostListDTO struct {
	TotalCount   int             `json:"total_count"`
	PendingCount int             `json:"pending_count"`
	Hosts        []RunnerHostDTO `json:"hosts"`
	At           time.Time       `json:"at"`
}

func runnerHostDTO(h *model.RunnerHost) RunnerHostDTO {
	labels := h.Labels
	if labels == nil {
		labels = []string{}
	}
	return RunnerHostDTO{
		Fingerprint: h.Fingerprint, State: string(h.State), Name: h.Name,
		OS: h.OS, Arch: h.Arch, Version: h.Version, Labels: labels,
		EnrolledFrom: h.EnrolledFrom, EnrolledAt: h.EnrolledAt,
		ApprovedBy: h.ApprovedBy, ApprovedAt: h.ApprovedAt,
		LastSeenAt: h.LastSeenAt, Note: h.Note,
	}
}

func (s *Server) listRunnerHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.cfg.Store.ListRunnerHosts(r.Context())
	if err != nil {
		storeErr(w, "list runner hosts", err)
		return
	}
	out := RunnerHostListDTO{TotalCount: len(hosts), Hosts: make([]RunnerHostDTO, 0, len(hosts)), At: s.now()}
	for _, h := range hosts {
		if h.State == model.RunnerHostPending {
			out.PendingCount++
		}
		out.Hosts = append(out.Hosts, runnerHostDTO(h))
	}
	writeJSON(w, http.StatusOK, out)
}

// runnerHostAction is the body of an approve or revoke.
type runnerHostAction struct {
	// Note is the operator's own words: which machine this is, or why it was
	// revoked.
	Note string `json:"note"`
	// Labels, when set, replaces what this host's runners may claim.
	Labels []string `json:"labels"`
}

// approveRunnerHost lets a machine take jobs. This is the one action in the
// platform that hands out execution on the runner fleet, so it records the
// account that took it.
func (s *Server) approveRunnerHost(w http.ResponseWriter, r *http.Request) {
	s.setRunnerHostState(w, r, model.RunnerHostApproved)
}

// revokeRunnerHost takes it away again. The row stays, so the same key cannot
// come back as a fresh pending host.
func (s *Server) revokeRunnerHost(w http.ResponseWriter, r *http.Request) {
	s.setRunnerHostState(w, r, model.RunnerHostRevoked)
}

func (s *Server) setRunnerHostState(w http.ResponseWriter, r *http.Request, state model.RunnerHostState) {
	fingerprint := r.PathValue("fingerprint")
	var body runnerHostAction
	if r.ContentLength > 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "malformed body: %v", err)
			return
		}
	}

	actor := s.cfg.Actor(r)
	host, err := s.cfg.Store.SetRunnerHostState(r.Context(), fingerprint, state, actor, body.Note, s.now())
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "no runner host has the fingerprint %q", fingerprint)
		return
	}
	if err != nil {
		storeErr(w, "set runner host state", err)
		return
	}
	if body.Labels != nil {
		host, err = s.cfg.Store.SetRunnerHostLabels(r.Context(), fingerprint, body.Labels)
		if err != nil {
			storeErr(w, "set runner host labels", err)
			return
		}
	}

	// Approving a host is the kind of thing somebody asks about a year later,
	// so it goes in the event log rather than only in a row that records the
	// current state.
	if err := s.cfg.Store.RecordEvent(r.Context(), store.Event{
		At:      s.now(),
		Kind:    "runner_host." + string(state),
		Message: actor + " " + string(state) + " runner host " + fingerprint,
		Detail: map[string]any{
			"fingerprint": fingerprint, "actor": actor, "note": body.Note, "name": host.Name,
		},
	}); err != nil {
		storeErr(w, "record runner host event", err)
		return
	}
	writeJSON(w, http.StatusOK, runnerHostDTO(host))
}
