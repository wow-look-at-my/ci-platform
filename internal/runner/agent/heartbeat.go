package agent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
)

// heartbeat extends the lease while a job runs and carries control-plane
// instructions back into it. It is the only path by which a cancellation
// reaches a running job.
type heartbeat struct {
	mu     sync.Mutex
	cancel *model.CancelReason
	lost   bool

	stopCh chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
}

func (h *heartbeat) stop() {
	h.once.Do(func() { close(h.stopCh) })
	h.wg.Wait()
}

func (h *heartbeat) cancelReason() *model.CancelReason {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cancel
}

func (h *heartbeat) leaseLost() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lost
}

// startHeartbeat begins the ticker. On a cancel instruction it records the
// reason, writes it into the job log, and stops the job; on a lost lease it
// stops the job immediately and marks that no result may be reported.
func (a *Agent) startHeartbeat(ctx context.Context, asg *protocol.Assignment, sink *LogSink, cancelJob context.CancelFunc, log *slog.Logger) *heartbeat {
	h := &heartbeat{stopCh: make(chan struct{})}
	interval := a.heartbeatInterval()

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-h.stopCh:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}

			resp, err := a.cfg.Client.Heartbeat(ctx, protocol.HeartbeatRequest{
				RunnerID: a.cfg.RunnerID, JobID: asg.JobID, Attempt: asg.Attempt, Phase: "execute",
			})
			if err != nil {
				// A failed heartbeat is infra and is logged; the control plane
				// reaps the lease if they keep failing.
				log.Error("heartbeat failed", "err", err, "class", classOf(err))
				continue
			}
			if resp.LeaseLost {
				h.mu.Lock()
				h.lost = true
				h.mu.Unlock()
				sink.Line(0, "platform", "", "the control plane reported this job's lease as lost; stopping without reporting a result")
				cancelJob()
				return
			}
			if resp.Cancel != nil {
				reason := *resp.Cancel
				if err := reason.Validate(); err != nil {
					// A cancellation with no explanation is the incident this
					// platform exists to prevent; it is recorded as a defect
					// rather than passed through silently.
					reason.Sentence = "the control plane cancelled this job without recording a reason, which is a control-plane defect"
					if !reason.Actor.Valid() {
						reason.Actor = model.CancelActorUser
					}
					log.Error("cancellation arrived without a valid reason", "err", err)
				}
				h.mu.Lock()
				h.cancel = &reason
				h.mu.Unlock()
				sink.Line(0, "platform", "", "job cancelled by "+string(reason.Actor)+": "+reason.Sentence)
				log.Info("job cancelled", "actor", reason.Actor, "reason", reason.Sentence)
				cancelJob()
				return
			}
		}
	}()
	return h
}
