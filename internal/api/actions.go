package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// cancelRequest is the body of both cancel endpoints.
type cancelRequest struct {
	Reason string `json:"reason"`
	// Actor overrides the request-derived principal, for a CLI that knows who
	// it is acting for.
	Actor string `json:"actor,omitempty"`
}

// actionResponse confirms what was recorded, so the caller can show it back.
type actionResponse struct {
	OK     bool       `json:"ok"`
	Action string     `json:"action"`
	RunID  int64      `json:"run_id,omitempty"`
	JobID  int64      `json:"job_id,omitempty"`
	Actor  string     `json:"actor"`
	Cancel *CancelDTO `json:"cancel,omitempty"`
}

// decodeCancel builds the CancelReason. Actor is always "user" here: this
// endpoint exists only for a human pressing cancel, and every other actor
// (timeout, concurrency group, runner loss) is recorded by the scheduler.
// A cancel with no sentence is rejected, because "cancelled" with no reason
// anywhere is the incident this platform exists to never repeat.
func (s *Server) decodeCancel(w http.ResponseWriter, r *http.Request) (model.CancelReason, bool) {
	var req cancelRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "could not read request body: %v", err)
		return model.CancelReason{}, false
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "request body must be JSON {\"reason\": \"...\"}: %v", err)
			return model.CancelReason{}, false
		}
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		writeErr(w, http.StatusBadRequest, "missing_reason",
			"cancel requires a non-empty \"reason\": the reason is shown verbatim to whoever asks why this was cancelled")
		return model.CancelReason{}, false
	}
	actor := strings.TrimSpace(req.Actor)
	if actor == "" {
		actor = s.cfg.Actor(r)
	}
	cr := model.CancelReason{
		Actor:       model.CancelActorUser,
		Sentence:    "Cancelled by " + actor + ": " + reason,
		TriggeredBy: actor,
	}
	if err := cr.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "%v", err)
		return model.CancelReason{}, false
	}
	return cr, true
}

// controller returns the wired controller, or writes a 503 naming the gap.
func (s *Server) controller(w http.ResponseWriter) (Controller, bool) {
	if s.cfg.Controller == nil {
		writeErr(w, http.StatusServiceUnavailable, "no_controller",
			"no scheduler is wired into this API instance, so cancel and re-run cannot be performed")
		return nil, false
	}
	return s.cfg.Controller, true
}

func controlErr(w http.ResponseWriter, op string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "%s: not found", op)
		return
	}
	writeErr(w, http.StatusInternalServerError, "controller_error", "%s: %v", op, err)
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ctrl, ok := s.controller(w)
	if !ok {
		return
	}
	reason, ok := s.decodeCancel(w, r)
	if !ok {
		return
	}
	if err := ctrl.Cancel(r.Context(), id, reason); err != nil {
		controlErr(w, "cancel run", err)
		return
	}
	writeJSON(w, http.StatusAccepted, actionResponse{
		OK: true, Action: "cancel", RunID: id, Actor: reason.TriggeredBy, Cancel: cancelDTO(&reason),
	})
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ctrl, ok := s.controller(w)
	if !ok {
		return
	}
	reason, ok := s.decodeCancel(w, r)
	if !ok {
		return
	}
	if err := ctrl.CancelJob(r.Context(), id, reason); err != nil {
		controlErr(w, "cancel job", err)
		return
	}
	writeJSON(w, http.StatusAccepted, actionResponse{
		OK: true, Action: "cancel", JobID: id, Actor: reason.TriggeredBy, Cancel: cancelDTO(&reason),
	})
}

func (s *Server) rerunRun(w http.ResponseWriter, r *http.Request) { s.rerun(w, r, "rerun") }
func (s *Server) rerunRunFailed(w http.ResponseWriter, r *http.Request) {
	s.rerun(w, r, "rerun-failed")
}

func (s *Server) rerun(w http.ResponseWriter, r *http.Request, action string) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ctrl, ok := s.controller(w)
	if !ok {
		return
	}
	actor := s.cfg.Actor(r)
	var err error
	if action == "rerun" {
		err = ctrl.Rerun(r.Context(), id, actor)
	} else {
		err = ctrl.RerunFailed(r.Context(), id, actor)
	}
	if err != nil {
		controlErr(w, action+" run", err)
		return
	}
	writeJSON(w, http.StatusAccepted, actionResponse{OK: true, Action: action, RunID: id, Actor: actor})
}

func (s *Server) rerunJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ctrl, ok := s.controller(w)
	if !ok {
		return
	}
	actor := s.cfg.Actor(r)
	if err := ctrl.RerunJob(r.Context(), id, actor); err != nil {
		controlErr(w, "rerun job", err)
		return
	}
	writeJSON(w, http.StatusAccepted, actionResponse{OK: true, Action: "rerun", JobID: id, Actor: actor})
}
