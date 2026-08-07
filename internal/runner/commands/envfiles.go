package commands

import (
	"fmt"
	"path"
	"strings"
)

// EnvFiles are the five files a step may write to communicate with the runner.
// They are created fresh per step and read back after it exits, so one step
// cannot see another's unflushed writes.
type EnvFiles struct {
	Output      string
	Env         string
	Path        string
	StepSummary string
	State       string
}

// NewEnvFiles names the five files for one step under dir. key must be unique
// per step (attempt included) so a retry never reads the previous attempt's
// values.
func NewEnvFiles(dir, key string) EnvFiles {
	return EnvFiles{
		Output:      path.Join(dir, "output_"+key),
		Env:         path.Join(dir, "env_"+key),
		Path:        path.Join(dir, "path_"+key),
		StepSummary: path.Join(dir, "summary_"+key),
		State:       path.Join(dir, "state_"+key),
	}
}

// EnvMap is the environment the step needs to find its files.
func (f EnvFiles) EnvMap() map[string]string {
	return map[string]string{
		"GITHUB_OUTPUT":       f.Output,
		"GITHUB_ENV":          f.Env,
		"GITHUB_PATH":         f.Path,
		"GITHUB_STEP_SUMMARY": f.StepSummary,
		"GITHUB_STATE":        f.State,
	}
}

// All lists the five paths, in creation order.
func (f EnvFiles) All() []string {
	return []string{f.Output, f.Env, f.Path, f.StepSummary, f.State}
}

// KeyValues is a parsed env file, keeping insertion order because later writes
// of the same key win and callers apply them in order.
type KeyValues struct {
	Order  []string
	Values map[string]string
}

// Get returns a value and whether it was present.
func (kv KeyValues) Get(k string) (string, bool) {
	v, ok := kv.Values[k]
	return v, ok
}

// ParseKeyValues reads the $GITHUB_OUTPUT / $GITHUB_ENV / $GITHUB_STATE format:
// either "k=v" on one line, or a heredoc:
//
//	k<<EOF
//	line one
//	line two
//	EOF
//
// Which form a line uses is decided by whether "=" or "<<" comes first, so a
// value containing "<<" and a delimiter containing "=" both behave. Neither key
// nor value is trimmed.
//
// A malformed file is an error, never a partial silent read: a step that wrote
// half an output has a bug its author needs to see.
func ParseKeyValues(data string) (KeyValues, error) {
	out := KeyValues{Values: map[string]string{}}
	lines := splitLines(data)
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if line == "" {
			continue
		}
		eq := strings.Index(line, "=")
		hd := strings.Index(line, "<<")
		switch {
		case eq >= 0 && (hd < 0 || eq < hd):
			key, value := line[:eq], line[eq+1:]
			if key == "" {
				return out, fmt.Errorf("env file: invalid format %q: name must not be empty", truncate(line, 60))
			}
			out.set(key, value)
		case hd >= 0:
			key, delim := line[:hd], line[hd+2:]
			if key == "" || delim == "" {
				return out, fmt.Errorf("env file: invalid format %q: name and delimiter must not be empty", truncate(line, 60))
			}
			body, next, err := readHeredoc(lines, i+1, key, delim)
			if err != nil {
				return out, err
			}
			out.set(key, body)
			i = next
		default:
			return out, fmt.Errorf("env file: invalid format %q: neither k=v nor a heredoc header", truncate(line, 60))
		}
	}
	return out, nil
}

// readHeredoc consumes the value lines up to the closing delimiter and returns
// the index of that delimiter line.
func readHeredoc(lines []string, start int, key, delim string) (string, int, error) {
	var body []string
	for i := start; i < len(lines); i++ {
		if lines[i] == delim {
			return strings.Join(body, "\n"), i, nil
		}
		if strings.Contains(lines[i], delim) {
			// @actions/core refuses to write a value containing its own
			// delimiter; accepting it here would let a value close the block
			// early and smuggle in extra keys.
			return "", 0, fmt.Errorf("env file: key %q has delimiter %q inside its value", key, delim)
		}
		body = append(body, lines[i])
	}
	return "", 0, fmt.Errorf("env file: key %q: matching delimiter %q not found", key, delim)
}

func (kv *KeyValues) set(k, v string) {
	if _, seen := kv.Values[k]; !seen {
		kv.Order = append(kv.Order, k)
	}
	kv.Values[k] = v
}

// ParsePathFile reads $GITHUB_PATH: one directory per line, blanks ignored.
func ParsePathFile(data string) []string {
	var out []string
	for _, l := range splitLines(data) {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
