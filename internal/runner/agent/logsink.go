package agent

import (
	"context"
	"sync"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
	"github.com/wow-look-at-my/ci-platform/internal/runner/mask"
)

// LogSink batches log lines to the control plane. A POST per line would swamp
// it on a chatty build, so lines accumulate until the buffer is full or the
// flush interval passes.
//
// Every line is masked here, on the way in: nothing unmasked is ever held in
// the buffer, so a crash mid-flush cannot leak a secret either.
type LogSink struct {
	client   ControlPlane
	runnerID string
	jobID    int64
	attempt  int
	masker   *mask.Masker

	interval time.Duration
	maxLines int
	now      func() time.Time

	mu      sync.Mutex
	seq     int64
	buf     []model.LogLine
	errored error

	stop   chan struct{}
	closed bool
	wg     sync.WaitGroup
	// OnError is called when a batch cannot be delivered, so a failure to ship
	// logs is visible rather than silently dropped.
	OnError func(error)
}

// LogSinkConfig configures the sink.
type LogSinkConfig struct {
	Client   ControlPlane
	RunnerID string
	JobID    int64
	Attempt  int
	Masker   *mask.Masker
	Interval time.Duration
	MaxLines int
	Now      func() time.Time
	OnError  func(error)
}

// NewLogSink returns a started sink; call Close to flush and stop it.
func NewLogSink(cfg LogSinkConfig) *LogSink {
	if cfg.Interval <= 0 {
		cfg.Interval = 2 * time.Second
	}
	if cfg.MaxLines <= 0 {
		cfg.MaxLines = 200
	}
	if cfg.Masker == nil {
		cfg.Masker = mask.New()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &LogSink{
		client: cfg.Client, runnerID: cfg.RunnerID, jobID: cfg.JobID, attempt: cfg.Attempt,
		masker: cfg.Masker, interval: cfg.Interval, maxLines: cfg.MaxLines, now: cfg.Now,
		stop: make(chan struct{}), OnError: cfg.OnError,
	}
	return s
}

// Start runs the periodic flusher.
func (s *LogSink) Start(ctx context.Context) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(s.interval)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				s.flush(ctx)
			}
		}
	}()
}

// Line records one line, assigning its sequence number.
func (s *LogSink) Line(stepNumber int, stream, group, text string) {
	s.mu.Lock()
	s.seq++
	s.buf = append(s.buf, model.LogLine{
		Seq:        s.seq,
		Timestamp:  s.now(),
		StepNumber: stepNumber,
		Stream:     stream,
		Text:       s.masker.Mask(text),
		Group:      group,
	})
	full := len(s.buf) >= s.maxLines
	s.mu.Unlock()
	if full {
		s.flush(context.Background())
	}
}

// Seq is the last assigned sequence number, used as a step's log anchor.
func (s *LogSink) Seq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// Flush ships whatever is buffered.
func (s *LogSink) Flush(ctx context.Context) error {
	return s.flush(ctx)
}

func (s *LogSink) flush(ctx context.Context) error {
	s.mu.Lock()
	if len(s.buf) == 0 {
		s.mu.Unlock()
		return nil
	}
	batch := s.buf
	s.buf = nil
	s.mu.Unlock()

	err := s.client.Logs(ctx, protocol.LogBatch{
		RunnerID: s.runnerID, JobID: s.jobID, Attempt: s.attempt, Lines: batch,
	})
	if err != nil {
		s.mu.Lock()
		s.errored = err
		s.mu.Unlock()
		if s.OnError != nil {
			s.OnError(err)
		}
	}
	return err
}

// Err is the last delivery failure, if any.
func (s *LogSink) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.errored
}

// Close stops the flusher and ships the remainder.
func (s *LogSink) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	close(s.stop)
	s.wg.Wait()
	return s.flush(ctx)
}
