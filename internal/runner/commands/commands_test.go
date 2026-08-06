package commands

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

func TestParseCommandForms(t *testing.T) {
	tests := []struct {
		name   string
		line   string
		ok     bool
		cmd    string
		value  string
		params map[string]string
	}{
		{name: "plain text", line: "hello world", ok: false},
		{name: "no closing marker", line: "::error something", ok: false},
		{name: "empty name", line: ":: ::x", ok: false},
		{name: "no params", line: "::endgroup::", ok: true, cmd: "endgroup"},
		{name: "value only", line: "::group::Build stage", ok: true, cmd: "group", value: "Build stage"},
		{
			name: "params", line: "::error file=a.go,line=3,col=7,title=Boom::it broke",
			ok: true, cmd: "error", value: "it broke",
			params: map[string]string{"file": "a.go", "line": "3", "col": "7", "title": "Boom"},
		},
		{name: "leading whitespace", line: "   ::notice::hi", ok: true, cmd: "notice", value: "hi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, ok := Parse(tt.line)
			require.Equal(t, tt.ok, ok)
			if !tt.ok {
				return
			}
			assert.Equal(t, tt.cmd, cmd.Name)
			assert.Equal(t, tt.value, cmd.Value)
			for k, v := range tt.params {
				assert.Equal(t, v, cmd.Param(k), "param %s", k)
			}
		})
	}
}

func TestParseUnescapesDataAndProperties(t *testing.T) {
	cmd, ok := Parse("::error file=a%3Ab%2Cc,title=100%25::line one%0Aline two%0D%25")
	require.True(t, ok)
	assert.Equal(t, "a:b,c", cmd.Param("file"))
	assert.Equal(t, "100%", cmd.Param("title"))
	assert.Equal(t, "line one\nline two\r%", cmd.Value)
}

func TestEscapeRoundTrip(t *testing.T) {
	msg := "50% failed\nsecond line\r"
	cmd, ok := Parse(ErrorLine(msg))
	require.True(t, ok)
	assert.Equal(t, msg, cmd.Value)
}

type capture struct {
	annotations []model.Annotation
	masks       []string
	outputs     map[string]string
	states      map[string]string
	debug       []string
	unknown     []Command
}

func newCapture() (*capture, Handler) {
	c := &capture{outputs: map[string]string{}, states: map[string]string{}}
	return c, Handler{
		Annotation: func(a model.Annotation) { c.annotations = append(c.annotations, a) },
		Mask:       func(v string) { c.masks = append(c.masks, v) },
		Output:     func(k, v string) { c.outputs[k] = v },
		State:      func(k, v string) { c.states[k] = v },
		Debug:      func(m string) { c.debug = append(c.debug, m) },
		Unknown:    func(cmd Command) { c.unknown = append(c.unknown, cmd) },
	}
}

func TestProcessorAnnotations(t *testing.T) {
	c, h := newCapture()
	p := NewProcessor(h)

	out, _, emit := p.Line("::error file=main.go,line=12,col=4,endColumn=9,title=Vet::undefined x")
	assert.True(t, emit)
	assert.Equal(t, "Error: undefined x", out)
	require.Len(t, c.annotations, 1)
	a := c.annotations[0]
	assert.Equal(t, "main.go", a.Path)
	assert.Equal(t, 12, a.StartLine)
	assert.Equal(t, 12, a.EndLine, "endLine defaults to line")
	assert.Equal(t, 4, a.StartCol)
	assert.Equal(t, 9, a.EndCol)
	assert.Equal(t, model.AnnotationFailure, a.Level)
	assert.Equal(t, "Vet", a.Title)

	_, _, _ = p.Line("::warning::careful")
	_, _, _ = p.Line("::notice::fyi")
	require.Len(t, c.annotations, 3)
	assert.Equal(t, model.AnnotationWarning, c.annotations[1].Level)
	assert.Equal(t, model.AnnotationNotice, c.annotations[2].Level)
}

func TestProcessorGroups(t *testing.T) {
	c, h := newCapture()
	p := NewProcessor(h)

	_, _, emit := p.Line("::group::Install")
	assert.False(t, emit)
	assert.Equal(t, "Install", p.Group())

	_, group, emit := p.Line("installing...")
	assert.True(t, emit)
	assert.Equal(t, "Install", group)

	_, _, _ = p.Line("::group::Nested")
	assert.Equal(t, "Nested", p.Group())
	_, _, _ = p.Line("::endgroup::")
	assert.Equal(t, "Install", p.Group())
	_, _, _ = p.Line("::endgroup::")
	assert.Equal(t, "", p.Group())
	// An unbalanced endgroup must not panic or go negative.
	_, _, _ = p.Line("::endgroup::")
	assert.Equal(t, "", p.Group())
	assert.Empty(t, c.annotations)
}

func TestProcessorAddMaskAndDeprecatedCommands(t *testing.T) {
	c, h := newCapture()
	p := NewProcessor(h)

	_, _, emit := p.Line("::add-mask::s3cr3t")
	assert.False(t, emit, "an add-mask line must not be echoed; it contains the secret")
	assert.Equal(t, []string{"s3cr3t"}, c.masks)

	out, _, emit := p.Line("::set-output name=sha::abc123")
	assert.True(t, emit)
	assert.Contains(t, out, "deprecated")
	assert.Equal(t, "abc123", c.outputs["sha"])

	out, _, emit = p.Line("::save-state name=pid::42")
	assert.True(t, emit)
	assert.Contains(t, out, "deprecated")
	assert.Equal(t, "42", c.states["pid"])
}

func TestProcessorStopAndResume(t *testing.T) {
	c, h := newCapture()
	p := NewProcessor(h)

	_, _, emit := p.Line("::stop-commands::a1b2c3d4e5f6")
	assert.False(t, emit)
	assert.True(t, p.Stopped())
	assert.Contains(t, c.masks, "a1b2c3d4e5f6", "a long stop token is masked")

	out, _, emit := p.Line("::error::this must not become an annotation")
	assert.True(t, emit)
	assert.Equal(t, "::error::this must not become an annotation", out)
	assert.Empty(t, c.annotations)

	_, _, emit = p.Line("::a1b2c3d4e5f6::")
	assert.False(t, emit)
	assert.False(t, p.Stopped())

	_, _, _ = p.Line("::error::now it counts")
	assert.Len(t, c.annotations, 1)
}

func TestProcessorRejectsGuessableStopTokens(t *testing.T) {
	for _, token := range []string{"", "pause-logging", "error", "add-mask"} {
		t.Run("token="+token, func(t *testing.T) {
			_, h := newCapture()
			p := NewProcessor(h)
			out, _, emit := p.Line("::stop-commands::" + token)
			assert.True(t, emit)
			assert.Contains(t, out, "Error:")
			assert.False(t, p.Stopped(), "an invalid token must not pause command processing")
		})
	}
}

func TestProcessorUnknownCommandIsEchoedNotSwallowed(t *testing.T) {
	c, h := newCapture()
	p := NewProcessor(h)
	out, _, emit := p.Line("::add-matcher::foo.json")
	assert.True(t, emit)
	assert.Equal(t, "::add-matcher::foo.json", out)
	require.Len(t, c.unknown, 1)
	assert.Equal(t, "add-matcher", c.unknown[0].Name)
}

func TestProcessorEchoAndDebug(t *testing.T) {
	c, h := newCapture()
	p := NewProcessor(h)
	_, _, emit := p.Line("::echo::on")
	assert.False(t, emit)
	assert.True(t, p.Echo())
	_, _, _ = p.Line("::echo::off")
	assert.False(t, p.Echo())

	out, _, emit := p.Line("::debug::inner state")
	assert.True(t, emit)
	assert.Equal(t, "Debug: inner state", out)
	assert.Equal(t, []string{"inner state"}, c.debug)
}

func TestProcessorNilHandlerFieldsAreSafe(t *testing.T) {
	p := NewProcessor(Handler{})
	require.NotPanics(t, func() {
		p.Line("::error::x")
		p.Line("::add-mask::y")
		p.Line("::set-output name=a::b")
		p.Line("::save-state name=a::b")
		p.Line("::debug::d")
		p.Line("::whatever::z")
	})
}
