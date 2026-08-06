// Package mask redacts secret values from log text before it leaves the runner
// process. Every rendering a build is likely to produce is registered, not just
// the literal value: a secret echoed through base64 or a URL query string is
// still the secret.
package mask

import (
	"encoding/base64"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// Placeholder is what a redacted value is replaced with.
const Placeholder = "***"

// MinLength skips values too short to redact usefully. Masking a 1-character
// secret would black out most of a build log while hiding nothing.
const MinLength = 3

// Masker holds the registered secret values and their renderings.
//
// It is safe for concurrent use: log lines are masked on the emitting
// goroutine while ::add-mask:: registrations arrive from output parsing.
type Masker struct {
	mu       sync.RWMutex
	values   map[string]struct{}
	replacer *strings.Replacer
}

// New returns an empty masker.
func New() *Masker {
	return &Masker{values: make(map[string]struct{})}
}

// Add registers a secret value and every rendering of it that could appear in
// output. A multi-line secret is also registered line by line, because a build
// that prints it through a tool may emit only one of its lines.
func (m *Masker) Add(secret string) {
	if len(secret) < MinLength {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range renderings(secret) {
		if len(v) < MinLength {
			continue
		}
		m.values[v] = struct{}{}
	}
	m.replacer = nil
}

// AddAll registers every value in the map, ignoring the keys. Secret names are
// not secret; their values are.
func (m *Masker) AddAll(values map[string]string) {
	for _, v := range values {
		m.Add(v)
	}
}

// Mask replaces every registered value with Placeholder.
func (m *Masker) Mask(s string) string {
	if s == "" {
		return s
	}
	r := m.build()
	if r == nil {
		return s
	}
	return r.Replace(s)
}

// Count reports how many distinct renderings are registered.
func (m *Masker) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.values)
}

func (m *Masker) build() *strings.Replacer {
	m.mu.RLock()
	r := m.replacer
	n := len(m.values)
	m.mu.RUnlock()
	if r != nil || n == 0 {
		return r
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.replacer != nil {
		return m.replacer
	}
	vals := make([]string, 0, len(m.values))
	for v := range m.values {
		vals = append(vals, v)
	}
	// Longest first: strings.Replacer prefers the earliest-listed pattern at a
	// given position, so a value that is a prefix of another must not win.
	sort.Slice(vals, func(i, j int) bool {
		if len(vals[i]) != len(vals[j]) {
			return len(vals[i]) > len(vals[j])
		}
		return vals[i] < vals[j]
	})
	pairs := make([]string, 0, len(vals)*2)
	for _, v := range vals {
		pairs = append(pairs, v, Placeholder)
	}
	m.replacer = strings.NewReplacer(pairs...)
	return m.replacer
}

// renderings returns the forms of a secret that can appear in output: the raw
// value, its individual lines, base64 in all four alphabets, and the two URL
// escapings.
func renderings(secret string) []string {
	seeds := []string{secret}
	trimmed := strings.TrimSpace(secret)
	if trimmed != secret {
		seeds = append(seeds, trimmed)
	}
	if strings.ContainsAny(secret, "\r\n") {
		for _, line := range strings.FieldsFunc(secret, func(r rune) bool { return r == '\n' || r == '\r' }) {
			if line = strings.TrimSpace(line); line != "" {
				seeds = append(seeds, line)
			}
		}
	}

	out := make([]string, 0, len(seeds)*6)
	seen := make(map[string]struct{}, len(seeds)*6)
	add := func(v string) {
		if v == "" {
			return
		}
		if _, ok := seen[v]; ok {
			return
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	for _, s := range seeds {
		add(s)
		b := []byte(s)
		add(base64.StdEncoding.EncodeToString(b))
		add(base64.RawStdEncoding.EncodeToString(b))
		add(base64.URLEncoding.EncodeToString(b))
		add(base64.RawURLEncoding.EncodeToString(b))
		add(url.QueryEscape(s))
		add(url.PathEscape(s))
	}
	return out
}
