// Package logstore holds job logs: append-only, one log per (job, attempt),
// live-streamed to watchers and sealed into the blob store when the attempt
// ends.
//
// A re-run never overwrites the previous attempt's log, because "the log I was
// reading was replaced by the retry" is exactly the failure this platform
// exists to avoid. ANSI escapes are stored verbatim; the UI renders them.
package logstore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/blob"
	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// ErrNotFound is returned for a (job, attempt) with no log at all.
var ErrNotFound = errors.New("logstore: no log for that job attempt")

// ErrSealed is returned when a finalized attempt is appended to.
var ErrSealed = errors.New("logstore: attempt is finalized; its log is append-only and closed")

// FellBehindText is the line a dropped subscriber receives as its last. It is
// a line rather than a silent close so the client knows to reconnect and from
// where, instead of believing the job simply stopped logging.
const FellBehindText = "log stream fell behind, reconnect"

// Store is the log surface every other package consumes.
type Store interface {
	Append(ctx context.Context, jobID int64, attempt int, lines []model.LogLine) (lastSeq int64, err error)
	Read(ctx context.Context, jobID int64, attempt int, fromSeq int64, limit int) ([]model.LogLine, error)
	// Subscribe streams lines from fromSeq, then live ones, until ctx ends.
	Subscribe(ctx context.Context, jobID int64, attempt int, fromSeq int64) (<-chan model.LogLine, error)
	Raw(ctx context.Context, jobID int64, attempt int) (io.ReadCloser, error)
	Finalize(ctx context.Context, jobID int64, attempt int) error
}

// Options configures a Log.
type Options struct {
	// Blob is where sealed logs land. Required: without it a restart loses
	// every finished job's log, which is not a mode worth offering.
	Blob blob.Store
	// SubscriberBuffer bounds each live subscriber's queue. A subscriber that
	// exceeds it is dropped with FellBehindText rather than allowed to stall
	// the writer.
	SubscriberBuffer int
	// KeyPrefix namespaces sealed logs in the blob store.
	KeyPrefix string
	Now       func() time.Time
}

// DefaultSubscriberBuffer is a few seconds of a chatty build.
const DefaultSubscriberBuffer = 512

// Log is the in-memory hot buffer plus blob-backed cold storage.
type Log struct {
	blob   blob.Store
	bufN   int
	prefix string
	now    func() time.Time

	mu    sync.Mutex
	attsp map[attemptKey]*attempt
}

var _ Store = (*Log)(nil)

type attemptKey struct {
	jobID   int64
	attempt int
}

type attempt struct {
	mu      sync.Mutex
	lines   []model.LogLine
	lastSeq int64
	sealed  bool
	subs    map[*subscriber]struct{}
}

// New validates opts and returns a Log.
func New(opts Options) (*Log, error) {
	if opts.Blob == nil {
		return nil, errors.New("logstore: a blob store is required; sealed logs must survive a restart")
	}
	l := &Log{
		blob:   opts.Blob,
		bufN:   opts.SubscriberBuffer,
		prefix: opts.KeyPrefix,
		now:    opts.Now,
		attsp:  map[attemptKey]*attempt{},
	}
	if l.bufN <= 0 {
		l.bufN = DefaultSubscriberBuffer
	}
	if l.prefix == "" {
		l.prefix = "logs"
	}
	if l.now == nil {
		l.now = time.Now
	}
	return l, nil
}

// SealedKey is the blob key a finalized attempt's log lives at.
func (l *Log) SealedKey(jobID int64, attempt int) string {
	return fmt.Sprintf("%s/%d/%d.jsonl", l.prefix, jobID, attempt)
}

func (l *Log) get(k attemptKey, create bool) (a *attempt, created bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a = l.attsp[k]
	if a == nil && create {
		a = &attempt{subs: map[*subscriber]struct{}{}}
		l.attsp[k] = a
		created = true
	}
	return a, created
}

func (l *Log) forget(k attemptKey) {
	l.mu.Lock()
	delete(l.attsp, k)
	l.mu.Unlock()
}

// sealedExists reports whether this attempt's log has already been sealed. A
// storage error is returned rather than read as "not sealed": guessing here
// would let a re-opened attempt shadow a finished job's log.
func (l *Log) sealedExists(ctx context.Context, jobID int64, att int) (bool, error) {
	_, err := l.blob.Stat(ctx, l.SealedKey(jobID, att))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, blob.ErrNotFound):
		return false, nil
	default:
		return false, fmt.Errorf("logstore: check sealed log for job %d attempt %d: %w", jobID, att, err)
	}
}

// Append records lines and assigns each a monotonic Seq. The caller's Seq
// values are replaced: sequence numbers are the store's to hand out, and they
// are the UI's deep-link anchor.
func (l *Log) Append(ctx context.Context, jobID int64, attempt int, lines []model.LogLine) (int64, error) {
	if err := validate(jobID, attempt); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	k := attemptKey{jobID, attempt}
	a, created := l.get(k, true)
	if created {
		// A hot buffer for an attempt that was already sealed would shadow the
		// sealed log, which is the one thing this store must never allow.
		sealed, err := l.sealedExists(ctx, jobID, attempt)
		if err != nil {
			l.forget(k)
			return 0, err
		}
		if sealed {
			l.forget(k)
			return 0, fmt.Errorf("%w: job %d attempt %d", ErrSealed, jobID, attempt)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sealed {
		return a.lastSeq, fmt.Errorf("%w: job %d attempt %d", ErrSealed, jobID, attempt)
	}
	for _, line := range lines {
		a.lastSeq++
		line.Seq = a.lastSeq
		if line.Timestamp.IsZero() {
			line.Timestamp = l.now()
		}
		a.lines = append(a.lines, line)
		a.fanout(line)
	}
	return a.lastSeq, nil
}

// fanout delivers to live subscribers. Callers hold a.mu.
func (a *attempt) fanout(line model.LogLine) {
	for s := range a.subs {
		if !s.push(line) {
			// The subscriber is too far behind to catch up. It is told so and
			// unregistered here; silently starving it would look identical to
			// a job that stopped producing output.
			delete(a.subs, s)
		}
	}
}

// Read returns up to limit lines with Seq > fromSeq.
func (l *Log) Read(ctx context.Context, jobID int64, attempt int, fromSeq int64, limit int) ([]model.LogLine, error) {
	if err := validate(jobID, attempt); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, fmt.Errorf("logstore: read limit must be positive, got %d", limit)
	}
	lines, err := l.snapshot(ctx, jobID, attempt)
	if err != nil {
		return nil, err
	}
	out := make([]model.LogLine, 0, limit)
	for _, line := range lines {
		if line.Seq <= fromSeq {
			continue
		}
		out = append(out, line)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// snapshot returns every line for an attempt, from memory when it is live and
// from the blob store when it has been sealed and evicted.
func (l *Log) snapshot(ctx context.Context, jobID int64, att int) ([]model.LogLine, error) {
	if a, _ := l.get(attemptKey{jobID, att}, false); a != nil {
		a.mu.Lock()
		if !a.sealed {
			out := make([]model.LogLine, len(a.lines))
			copy(out, a.lines)
			a.mu.Unlock()
			return out, nil
		}
		a.mu.Unlock()
	}
	rc, err := l.blob.Get(ctx, l.SealedKey(jobID, att))
	if errors.Is(err, blob.ErrNotFound) {
		return nil, fmt.Errorf("%w: job %d attempt %d", ErrNotFound, jobID, att)
	}
	if err != nil {
		return nil, fmt.Errorf("logstore: read sealed log for job %d attempt %d: %w", jobID, att, err)
	}
	defer rc.Close()

	var out []model.LogLine
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		var line model.LogLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			return nil, fmt.Errorf("logstore: sealed log for job %d attempt %d is corrupt at line %d: %w", jobID, att, len(out)+1, err)
		}
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("logstore: scan sealed log for job %d attempt %d: %w", jobID, att, err)
	}
	return out, nil
}

// Subscribe replays from fromSeq and then streams live lines. The channel
// closes when the attempt is finalized, when ctx ends, or when this subscriber
// falls too far behind, in which case FellBehindText arrives first.
//
// An attempt that has not logged yet gets a live stream, not an error: the job
// page opens when the job starts, which is before its first line.
func (l *Log) Subscribe(ctx context.Context, jobID int64, att int, fromSeq int64) (<-chan model.LogLine, error) {
	if err := validate(jobID, att); err != nil {
		return nil, err
	}
	a, _ := l.get(attemptKey{jobID, att}, false)
	if a == nil {
		// Either the attempt is finished (replay the sealed log and close) or
		// it has not logged its first line yet, which is the normal case for a
		// UI that opened the job page the moment the job started.
		sealed, err := l.sealedExists(ctx, jobID, att)
		if err != nil {
			return nil, err
		}
		if sealed {
			lines, err := l.snapshot(ctx, jobID, att)
			if err != nil {
				return nil, err
			}
			return replayOnly(ctx, lines, fromSeq), nil
		}
		a, _ = l.get(attemptKey{jobID, att}, true)
	}

	a.mu.Lock()
	if a.sealed {
		a.mu.Unlock()
		lines, err := l.snapshot(ctx, jobID, att)
		if err != nil {
			return nil, err
		}
		return replayOnly(ctx, lines, fromSeq), nil
	}
	backlog := make([]model.LogLine, 0, len(a.lines))
	for _, line := range a.lines {
		if line.Seq > fromSeq {
			backlog = append(backlog, line)
		}
	}
	s := newSubscriber(l.bufN, backlog)
	a.subs[s] = struct{}{}
	a.mu.Unlock()

	go s.run(ctx, func() {
		a.mu.Lock()
		delete(a.subs, s)
		idle := len(a.subs) == 0 && len(a.lines) == 0 && !a.sealed
		a.mu.Unlock()
		if idle {
			// A subscriber that arrived before the first line, and left before
			// one came, must not leave an entry behind for every id it named.
			l.forget(attemptKey{jobID, att})
		}
	})
	return s.out, nil
}

func replayOnly(ctx context.Context, lines []model.LogLine, fromSeq int64) <-chan model.LogLine {
	ch := make(chan model.LogLine)
	go func() {
		defer close(ch)
		for _, line := range lines {
			if line.Seq <= fromSeq {
				continue
			}
			select {
			case ch <- line:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// Raw streams the attempt's text, which is what the download-raw endpoint
// serves. ANSI escapes are included because they are part of the output.
func (l *Log) Raw(ctx context.Context, jobID int64, att int) (io.ReadCloser, error) {
	lines, err := l.snapshot(ctx, jobID, att)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		w := bufio.NewWriter(pw)
		for _, line := range lines {
			if _, err := w.WriteString(line.Text + "\n"); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		if err := w.Flush(); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.Close()
	}()
	return pr, nil
}

// Finalize seals the attempt and flushes it to the blob store. The attempt is
// only marked sealed once the bytes are durable: a failed flush leaves the log
// live and returns the reason, because a log that silently vanished on restart
// is worse than an upload that failed loudly.
func (l *Log) Finalize(ctx context.Context, jobID int64, att int) error {
	if err := validate(jobID, att); err != nil {
		return err
	}
	a, _ := l.get(attemptKey{jobID, att}, false)
	if a == nil {
		// Already sealed and evicted, or never existed. The blob decides which.
		sealed, err := l.sealedExists(ctx, jobID, att)
		if err != nil {
			return err
		}
		if sealed {
			return nil
		}
		return fmt.Errorf("%w: job %d attempt %d", ErrNotFound, jobID, att)
	}

	a.mu.Lock()
	if a.sealed {
		a.mu.Unlock()
		return nil
	}
	lines := make([]model.LogLine, len(a.lines))
	copy(lines, a.lines)
	a.mu.Unlock()

	var buf []byte
	for _, line := range lines {
		b, err := json.Marshal(line)
		if err != nil {
			return fmt.Errorf("logstore: encode log line %d for job %d attempt %d: %w", line.Seq, jobID, att, err)
		}
		buf = append(buf, b...)
		buf = append(buf, '\n')
	}
	if _, _, err := l.blob.Put(ctx, l.SealedKey(jobID, att), bytes.NewReader(buf)); err != nil {
		return fmt.Errorf("logstore: seal log for job %d attempt %d: %w", jobID, att, err)
	}

	a.mu.Lock()
	a.sealed = true
	a.lines = nil
	for s := range a.subs {
		s.close()
		delete(a.subs, s)
	}
	a.mu.Unlock()

	// Drop the hot buffer; reads now come from the blob, and a later Append
	// for this attempt is refused because the sealed object exists.
	l.forget(attemptKey{jobID, att})
	return nil
}

func validate(jobID int64, attempt int) error {
	if jobID <= 0 {
		return fmt.Errorf("logstore: job id %d is not valid", jobID)
	}
	if attempt <= 0 {
		return fmt.Errorf("logstore: attempt %d is not valid", attempt)
	}
	return nil
}
