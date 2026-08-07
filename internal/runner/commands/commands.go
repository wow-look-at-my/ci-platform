// Package commands parses the ::workflow command:: protocol a step writes to
// stdout, and the per-step env files it writes to disk.
package commands

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// Command is one parsed ::name key=value::message line.
type Command struct {
	Name   string
	Params map[string]string
	Value  string
}

// Param returns a parameter value, or "" when absent.
func (c Command) Param(k string) string { return c.Params[k] }

// intParam parses a numeric parameter, returning 0 when absent or malformed.
func (c Command) intParam(k string) int {
	n, err := strconv.Atoi(strings.TrimSpace(c.Params[k]))
	if err != nil {
		return 0
	}
	return n
}

const marker = "::"

// Parse recognizes a workflow command line. ok is false for ordinary output.
func Parse(line string) (Command, bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, marker) {
		return Command{}, false
	}
	rest := s[len(marker):]
	end := strings.Index(rest, marker)
	if end < 0 {
		return Command{}, false
	}
	head := rest[:end]
	value := rest[end+len(marker):]

	name, params := head, ""
	if i := strings.IndexByte(head, ' '); i >= 0 {
		name, params = head[:i], head[i+1:]
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Command{}, false
	}
	return Command{
		Name:   name,
		Params: parseParams(params),
		Value:  unescapeData(value),
	}, true
}

func parseParams(s string) map[string]string {
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = unescapeProperty(v)
	}
	return out
}

// unescapeData reverses the message escaping. %25 is decoded last so an
// encoded percent cannot be re-interpreted as the start of another escape.
func unescapeData(s string) string {
	s = strings.ReplaceAll(s, "%0D", "\r")
	s = strings.ReplaceAll(s, "%0A", "\n")
	return strings.ReplaceAll(s, "%25", "%")
}

func unescapeProperty(s string) string {
	s = strings.ReplaceAll(s, "%0D", "\r")
	s = strings.ReplaceAll(s, "%0A", "\n")
	s = strings.ReplaceAll(s, "%3A", ":")
	s = strings.ReplaceAll(s, "%2C", ",")
	return strings.ReplaceAll(s, "%25", "%")
}

// Handler receives the side effects of the commands a step emits. A nil field
// means the runner does not care about that command; it is still never
// silently swallowed, because the emitted log line reports it.
type Handler struct {
	Annotation func(model.Annotation)
	Mask       func(value string)
	Output     func(name, value string)
	State      func(name, value string)
	Debug      func(message string)
	Unknown    func(cmd Command)
}

// Processor consumes a step's output line by line, applying the command
// protocol: group nesting, masking, annotations, and the stop-commands pause.
type Processor struct {
	h         Handler
	groups    []string
	stopToken string
	echo      bool
}

// NewProcessor returns a processor bound to h.
func NewProcessor(h Handler) *Processor { return &Processor{h: h} }

// Group is the ::group:: title currently in effect, "" at top level.
func (p *Processor) Group() string {
	if len(p.groups) == 0 {
		return ""
	}
	return p.groups[len(p.groups)-1]
}

// Stopped reports whether command processing is paused by ::stop-commands::.
func (p *Processor) Stopped() bool { return p.stopToken != "" }

// Line consumes one line of step output and returns the text to log. emit is
// false when the line was consumed entirely by the command protocol.
func (p *Processor) Line(text string) (out string, group string, emit bool) {
	cmd, ok := Parse(text)
	if p.stopToken != "" {
		// Paused: only the matching resume token is interpreted; everything
		// else, commands included, is ordinary output.
		if ok && cmd.Name == p.stopToken {
			p.stopToken = ""
			return "", p.Group(), false
		}
		return text, p.Group(), true
	}
	if !ok {
		return text, p.Group(), true
	}

	switch strings.ToLower(cmd.Name) {
	case "error", "warning", "notice":
		p.annotate(cmd)
		return labelFor(cmd.Name) + ": " + cmd.Value, p.Group(), true
	case "group":
		p.groups = append(p.groups, cmd.Value)
		return "", p.Group(), false
	case "endgroup":
		if len(p.groups) > 0 {
			p.groups = p.groups[:len(p.groups)-1]
		}
		return "", p.Group(), false
	case "add-mask":
		if p.h.Mask != nil && cmd.Value != "" {
			p.h.Mask(cmd.Value)
		}
		return "", p.Group(), false
	case "set-output":
		name := cmd.Param("name")
		if p.h.Output != nil && name != "" {
			p.h.Output(name, cmd.Value)
		}
		return "Warning: ::set-output:: is deprecated; write to $GITHUB_OUTPUT instead", p.Group(), true
	case "save-state":
		name := cmd.Param("name")
		if p.h.State != nil && name != "" {
			p.h.State(name, cmd.Value)
		}
		return "Warning: ::save-state:: is deprecated; write to $GITHUB_STATE instead", p.Group(), true
	case "stop-commands":
		token := cmd.Value
		if err := validateStopToken(token); err != nil {
			// A guessable token lets untrusted output disable command
			// processing and then re-enable it at will (CVE-2020-15228), so an
			// invalid one fails the step rather than pausing.
			return "Error: " + err.Error(), p.Group(), true
		}
		if p.h.Mask != nil && len(token) > 6 {
			p.h.Mask(token)
		}
		p.stopToken = token
		return "", p.Group(), false
	case "echo":
		p.echo = strings.EqualFold(strings.TrimSpace(cmd.Value), "on")
		return "", p.Group(), false
	case "debug":
		if p.h.Debug != nil {
			p.h.Debug(cmd.Value)
		}
		return "Debug: " + cmd.Value, p.Group(), true
	default:
		if p.h.Unknown != nil {
			p.h.Unknown(cmd)
		}
		return text, p.Group(), true
	}
}

// Echo reports whether ::echo::on is in effect.
func (p *Processor) Echo() bool { return p.echo }

func (p *Processor) annotate(cmd Command) {
	if p.h.Annotation == nil {
		return
	}
	start := cmd.intParam("line")
	end := cmd.intParam("endLine")
	if end == 0 {
		end = start
	}
	startCol := cmd.intParam("col")
	if startCol == 0 {
		startCol = cmd.intParam("column")
	}
	endCol := cmd.intParam("endColumn")
	p.h.Annotation(model.Annotation{
		Path:      cmd.Param("file"),
		StartLine: start,
		EndLine:   end,
		StartCol:  startCol,
		EndCol:    endCol,
		Level:     levelFor(cmd.Name),
		Message:   cmd.Value,
		Title:     cmd.Param("title"),
	})
}

func levelFor(name string) model.AnnotationLevel {
	switch strings.ToLower(name) {
	case "error":
		return model.AnnotationFailure
	case "warning":
		return model.AnnotationWarning
	default:
		return model.AnnotationNotice
	}
}

func labelFor(name string) string {
	switch strings.ToLower(name) {
	case "error":
		return "Error"
	case "warning":
		return "Warning"
	default:
		return "Notice"
	}
}

// knownCommands are the names Parse dispatches on. A stop-commands token equal
// to one of them would be swallowed as that command instead of resuming.
var knownCommands = map[string]bool{
	"error": true, "warning": true, "notice": true,
	"group": true, "endgroup": true, "add-mask": true,
	"set-output": true, "save-state": true, "stop-commands": true,
	"echo": true, "debug": true,
}

func validateStopToken(token string) error {
	switch {
	case token == "":
		return fmt.Errorf("::stop-commands:: requires a resume token")
	case knownCommands[strings.ToLower(token)]:
		return fmt.Errorf("::stop-commands:: token %q is a workflow command name and cannot resume", token)
	case strings.EqualFold(token, "pause-logging"):
		return fmt.Errorf("::stop-commands:: token %q is guessable and is not accepted", token)
	}
	return nil
}

// Escape renders text so it survives a round trip through Parse, for the
// commands the runner itself emits.
func Escape(s string) string {
	s = strings.ReplaceAll(s, "%", "%25")
	s = strings.ReplaceAll(s, "\r", "%0D")
	return strings.ReplaceAll(s, "\n", "%0A")
}

// ErrorLine renders an ::error:: command for a message the runner produced.
func ErrorLine(msg string) string { return fmt.Sprintf("::error::%s", Escape(msg)) }
