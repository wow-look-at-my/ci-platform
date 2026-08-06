package logstore

import (
	"context"
	"sync"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// subscriber is one live watcher. It owns a bounded ring: the writer never
// blocks on a slow reader, and a reader that overflows the ring is cut off
// with an explicit line rather than quietly starved.
type subscriber struct {
	out chan model.LogLine

	mu      sync.Mutex
	buf     []model.LogLine
	cap     int
	lastSeq int64
	fell    bool
	closed  bool
	wake    chan struct{}
}

func newSubscriber(capacity int, backlog []model.LogLine) *subscriber {
	s := &subscriber{
		out:  make(chan model.LogLine),
		cap:  capacity,
		wake: make(chan struct{}, 1),
	}
	// The backlog is allowed to exceed the ring: it is a bounded, known
	// quantity the caller explicitly asked for.
	s.buf = append(s.buf, backlog...)
	return s
}

// push queues a line, returning false when the subscriber has fallen behind
// and must be dropped.
func (s *subscriber) push(line model.LogLine) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.fell {
		return false
	}
	if len(s.buf) >= s.cap {
		s.fell = true
		s.signal()
		return false
	}
	s.buf = append(s.buf, line)
	s.signal()
	return true
}

// close ends the stream cleanly, which is what finalizing an attempt does.
func (s *subscriber) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.signal()
}

// signal wakes the pump. Callers hold s.mu.
func (s *subscriber) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// run drains the ring into out until the context ends, the attempt is
// finalized, or the subscriber falls behind. unregister detaches it from the
// attempt so a dead subscriber is not fanned out to forever.
func (s *subscriber) run(ctx context.Context, unregister func()) {
	defer func() {
		unregister()
		close(s.out)
	}()
	for {
		s.mu.Lock()
		batch := s.buf
		s.buf = nil
		fell, closed := s.fell, s.closed
		s.mu.Unlock()

		for _, line := range batch {
			select {
			case s.out <- line:
				s.mu.Lock()
				s.lastSeq = line.Seq
				s.mu.Unlock()
			case <-ctx.Done():
				return
			}
		}
		if fell {
			s.mu.Lock()
			last := s.lastSeq
			s.mu.Unlock()
			select {
			case s.out <- model.LogLine{Seq: last, Stream: "platform", Text: FellBehindText}:
			case <-ctx.Done():
			}
			return
		}
		if closed && len(batch) == 0 {
			return
		}
		select {
		case <-s.wake:
		case <-ctx.Done():
			return
		}
	}
}
