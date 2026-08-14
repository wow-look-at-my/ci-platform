package runnerapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/enrol"
	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// enrol records a host's public key and answers with the fingerprint an
// operator now has to approve.
//
// It is deliberately reachable without a credential, because it is the step
// before a host has one. What stops it being a way in is that it grants
// nothing: the row it writes is pending, and a pending host is refused
// everywhere else.
func (s *Server) enrol(w http.ResponseWriter, r *http.Request) {
	var req protocol.EnrolRequest
	if !decode(w, r, &req) {
		return
	}
	if req.APIVersion != "" && req.APIVersion != protocol.APIVersion {
		writeErr(w, http.StatusBadRequest, "unsupported api version "+req.APIVersion)
		return
	}

	pub, err := enrol.ParsePublicKey(req.PublicKey)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	fingerprint := enrol.Fingerprint(pub)
	at := time.Unix(req.Timestamp, 0)

	// Proving possession of the private half is what makes the fingerprint mean
	// something. Without it, anybody could enrol a key they do not hold and the
	// operator would approve a machine that is not the one in front of them.
	if err := s.sessions.CheckFreshness(fingerprint, at, req.Nonce); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := enrol.Verify(pub, enrol.PurposeEnrol, fingerprint, at, req.Nonce, req.Signature); err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}

	// Enrolment has to answer without a credential, and a signature over a key
	// the sender generated proves only that they generated it. So anything that
	// can reach this route can mint fingerprints, and without a cap that is an
	// unbounded table and an approval page nobody can read.
	//
	// The cap is on PENDING hosts, so it never gets in the way of a fleet that
	// is actually approved, and clearing it is the same action the operator was
	// going to take anyway.
	if err := s.checkPendingCapacity(r.Context(), fingerprint); err != nil {
		writeErr(w, http.StatusTooManyRequests, err.Error())
		return
	}

	host, err := s.opts.Store.EnrolRunnerHost(r.Context(), &model.RunnerHost{
		Fingerprint: fingerprint, PublicKey: enrol.EncodePublicKey(pub),
		Name: req.Name, OS: req.OS, Arch: req.Arch, Version: req.Version,
		Labels: req.Labels, EnrolledFrom: remoteHost(r), EnrolledAt: s.opts.Now().UTC(),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// An operator who never hears about a waiting host never approves it, and
	// the runner looks broken for reasons nothing explains.
	s.log.Info("runner host enrolment", "fingerprint", host.Fingerprint, "name", host.Name,
		"state", host.State, "from", host.EnrolledFrom)

	writeJSON(w, http.StatusOK, protocol.EnrolResponse{
		Fingerprint: host.Fingerprint,
		State:       string(host.State),
		Message:     enrolMessage(host),
	})
}

// checkPendingCapacity refuses a NEW fingerprint once too many are already
// waiting. A host that has enrolled before is always let through, so a runner
// restarting is never turned away by somebody else's flood.
func (s *Server) checkPendingCapacity(ctx context.Context, fingerprint string) error {
	if _, err := s.opts.Store.GetRunnerHost(ctx, fingerprint); err == nil {
		return nil
	}
	hosts, err := s.opts.Store.ListRunnerHosts(ctx)
	if err != nil {
		return fmt.Errorf("could not check how many hosts are pending: %v", err)
	}
	pending := 0
	for _, h := range hosts {
		if h.State == model.RunnerHostPending {
			pending++
		}
	}
	if pending < s.opts.MaxPendingHosts {
		return nil
	}
	s.log.Warn("refused an enrolment because too many hosts are already waiting for approval",
		"pending", pending, "limit", s.opts.MaxPendingHosts, "fingerprint", fingerprint)
	return fmt.Errorf("%d hosts are already waiting for approval, which is the limit; "+
		"approve or revoke the pending ones before enrolling another", pending)
}

func enrolMessage(h *model.RunnerHost) string {
	switch h.State {
	case model.RunnerHostApproved:
		return "this host is approved"
	case model.RunnerHostRevoked:
		return "this host has been revoked by an operator: " + h.Note
	default:
		return "this host is waiting for an operator to approve the fingerprint " + h.Fingerprint
	}
}

// session exchanges a signature for a short-lived bearer token. This is where
// approval is enforced, and it is re-read from the store on every renewal, so
// revoking a host stops it within one token lifetime.
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	var req protocol.SessionRequest
	if !decode(w, r, &req) {
		return
	}
	host, err := s.opts.Store.GetRunnerHost(r.Context(), req.Fingerprint)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusUnauthorized, "this fingerprint has not been enrolled")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	at := time.Unix(req.Timestamp, 0)
	if err := s.sessions.CheckFreshness(req.Fingerprint, at, req.Nonce); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	pub, err := enrol.ParsePublicKey(host.PublicKey)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "stored public key is unreadable: "+err.Error())
		return
	}
	// The signature is checked before the approval state so that a wrong key
	// never learns whether a fingerprint is approved.
	if err := enrol.Verify(pub, enrol.PurposeSession, req.Fingerprint, at, req.Nonce, req.Signature); err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	if !host.Approved() {
		// Say plainly what is wrong. A runner that sits silent because nobody
		// pressed approve is the most likely thing to happen here, and it must
		// not look like a network fault.
		writeErr(w, http.StatusForbidden, "this host is "+string(host.State)+
			"; an operator has to approve the fingerprint "+host.Fingerprint+" before it can take jobs")
		return
	}

	token, sess := s.sessions.Mint(host.Fingerprint, req.RunnerID)
	if err := s.opts.Store.TouchRunnerHost(r.Context(), host.Fingerprint, s.opts.Now()); err != nil {
		s.log.Warn("could not record that a runner host was seen", "fingerprint", host.Fingerprint, "err", err)
	}
	writeJSON(w, http.StatusOK, protocol.SessionResponse{
		Token: token, ExpiresAt: sess.Expires, Labels: host.Labels,
	})
}

type hostKey struct{}

// caller is the authenticated host behind a request.
type caller struct {
	host *model.RunnerHost
	sess enrol.Session
}

func withHost(ctx context.Context, h *model.RunnerHost, s enrol.Session) context.Context {
	return context.WithValue(ctx, hostKey{}, caller{host: h, sess: s})
}

// CallerFrom returns the approved host behind a request, if the middleware put
// one there.
func CallerFrom(ctx context.Context) (*model.RunnerHost, enrol.Session, bool) {
	c, ok := ctx.Value(hostKey{}).(caller)
	if !ok {
		return nil, enrol.Session{}, false
	}
	return c.host, c.sess, true
}

// allowedLabels intersects what a runner asked for with what its host is
// approved to serve, and reports what was dropped so the drop is never silent.
func allowedLabels(ctx context.Context, asked []string) (kept, dropped []string) {
	host, _, ok := CallerFrom(ctx)
	if !ok || host == nil {
		return asked, nil
	}
	for _, l := range asked {
		if host.AllowsLabel(l) {
			kept = append(kept, l)
		} else {
			dropped = append(dropped, l)
		}
	}
	return kept, dropped
}

// remoteHost is the address the request came from, for an operator comparing an
// enrolment against the machine they expect it from. A proxy header is
// deliberately not consulted: anything can send one, and a wrong address here
// would be worse than none.
func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
