package exec

import (
	"bytes"
	"strings"
	"sync"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/runner/commands"
	"github.com/wow-look-at-my/ci-platform/internal/runner/mask"
)

// tail keeps the last n bytes of output, which is what the classifier reads. A
// build that prints a gigabyte must not cost a gigabyte of memory to classify.
type tail struct {
	buf   []byte
	limit int
}

func newTail(limit int) *tail { return &tail{limit: limit} }

func (t *tail) add(line string) {
	t.buf = append(t.buf, line...)
	t.buf = append(t.buf, '\n')
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
}

func (t *tail) String() string { return string(t.buf) }

// collector turns a step's two output streams into log lines: it splits them
// into lines, runs the workflow-command protocol, masks every line, and only
// then hands them to the log.
type collector struct {
	mu       sync.Mutex
	proc     *commands.Processor
	masker   *mask.Masker
	log      Log
	number   int
	tail     *tail
	annots   []model.Annotation
	outputs  map[string]string
	state    map[string]string
	pending  map[string]*bytes.Buffer
	maskFunc func(string)
}

func newCollector(number int, log Log, masker *mask.Masker) *collector {
	c := &collector{
		masker:  masker,
		log:     log,
		number:  number,
		tail:    newTail(8 << 10),
		outputs: map[string]string{},
		state:   map[string]string{},
		pending: map[string]*bytes.Buffer{},
	}
	c.proc = commands.NewProcessor(commands.Handler{
		Annotation: func(a model.Annotation) { c.annots = append(c.annots, a) },
		Mask:       func(v string) { masker.Add(v) },
		Output:     func(k, v string) { c.outputs[k] = v },
		State:      func(k, v string) { c.state[k] = v },
	})
	return c
}

// writer returns an io.Writer for one stream. Both streams share the command
// processor, because ::group:: opened on stdout closes on stderr.
func (c *collector) writer(stream string) *streamWriter {
	return &streamWriter{c: c, stream: stream}
}

func (c *collector) line(stream, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out, group, emit := c.proc.Line(text)
	if !emit {
		return
	}
	masked := c.masker.Mask(out)
	c.tail.add(masked)
	c.log.Line(c.number, stream, group, masked)
}

// emit writes a platform line into the same stream as step output, so the
// runner's own narration keeps its place in the log.
func (c *collector) emit(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log.Line(c.number, "platform", c.proc.Group(), c.masker.Mask(text))
}

// flush emits any partial trailing line: a step whose last write had no
// newline must not lose it.
func (c *collector) flush() {
	c.mu.Lock()
	pending := c.pending
	c.pending = map[string]*bytes.Buffer{}
	c.mu.Unlock()
	for stream, buf := range pending {
		if buf.Len() > 0 {
			c.line(stream, buf.String())
		}
	}
}

type streamWriter struct {
	c      *collector
	stream string
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.c.mu.Lock()
	buf, ok := w.c.pending[w.stream]
	if !ok {
		buf = &bytes.Buffer{}
		w.c.pending[w.stream] = buf
	}
	buf.Write(p)
	data := buf.String()
	idx := strings.LastIndexByte(data, '\n')
	if idx < 0 {
		w.c.mu.Unlock()
		return len(p), nil
	}
	complete := data[:idx]
	buf.Reset()
	buf.WriteString(data[idx+1:])
	w.c.mu.Unlock()

	for _, line := range strings.Split(complete, "\n") {
		w.c.line(w.stream, strings.TrimSuffix(line, "\r"))
	}
	return len(p), nil
}
