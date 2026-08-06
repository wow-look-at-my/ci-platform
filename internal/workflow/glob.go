package workflow

import (
	"fmt"
	"regexp"
	"strings"
)

// GitHub's filter patterns are not standard globs, and the difference is the
// kind that gets guessed wrong: `?` and `+` are QUANTIFIERS applied to the
// preceding character, not wildcards. The dialect, per GitHub's filter-pattern
// documentation and the validator in rhysd/actionlint:
//
//	*      zero or more characters, but never `/`
//	**     zero or more of any character, including `/`
//	?      zero or one of the PRECEDING character
//	+      one or more of the PRECEDING character
//	[abc]  one character from the set, ranges allowed
//	\x     a literal x
//	!...   at the start of a pattern, negates it
//
// A `+` or `?` at the start of a pattern, or following another special
// character, is a syntax error rather than something to interpret generously.

// Glob is a compiled filter pattern.
type Glob struct {
	raw    string
	neg    bool
	re     *regexp.Regexp
	source string
}

// Raw returns the pattern as written.
func (g *Glob) Raw() string { return g.raw }

// Negated reports whether the pattern began with `!`.
func (g *Glob) Negated() bool { return g.neg }

// Match reports whether s matches, ignoring negation. Callers combine matches
// in order, because a later pattern overrides an earlier one.
func (g *Glob) Match(s string) bool { return g.re.MatchString(s) }

// CompileGlob compiles one filter pattern.
func CompileGlob(pattern string) (*Glob, error) {
	if pattern == "" {
		return nil, fmt.Errorf("empty filter pattern")
	}
	g := &Glob{raw: pattern}
	body := pattern
	if strings.HasPrefix(body, "!") {
		g.neg = true
		body = body[1:]
		if body == "" {
			return nil, fmt.Errorf("filter pattern %q negates nothing", pattern)
		}
	}

	var b strings.Builder
	b.WriteString("^")
	// prevAtom is the regexp fragment a following `?` or `+` would quantify.
	// It is empty when the previous token cannot be quantified, which is what
	// makes `*?` and a leading `+` errors rather than silently accepted.
	prevAtom := ""
	flush := func() {
		b.WriteString(prevAtom)
		prevAtom = ""
	}

	rs := []rune(body)
	for i := 0; i < len(rs); i++ {
		switch c := rs[i]; c {
		case '*':
			flush()
			if i+1 < len(rs) && rs[i+1] == '*' {
				i++
				b.WriteString(".*")
			} else {
				b.WriteString("[^/]*")
			}
		case '?', '+':
			if prevAtom == "" {
				return nil, fmt.Errorf("filter pattern %q: %q must follow a normal character, not the start of the pattern or another special character",
					pattern, string(c))
			}
			b.WriteString("(?:" + prevAtom + ")" + string(c))
			prevAtom = ""
		case '[':
			flush()
			class, next, err := compileClass(rs, i, pattern)
			if err != nil {
				return nil, err
			}
			prevAtom = class
			i = next
		case '\\':
			flush()
			if i+1 >= len(rs) {
				return nil, fmt.Errorf("filter pattern %q ends with a trailing backslash", pattern)
			}
			i++
			prevAtom = regexp.QuoteMeta(string(rs[i]))
		default:
			flush()
			prevAtom = regexp.QuoteMeta(string(c))
		}
	}
	flush()
	b.WriteString("$")

	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("filter pattern %q is not usable: %w", pattern, err)
	}
	g.re = re
	g.source = b.String()
	return g, nil
}

func compileClass(rs []rune, start int, pattern string) (string, int, error) {
	var b strings.Builder
	b.WriteString("[")
	i := start + 1
	if i < len(rs) && rs[i] == '!' {
		// GitHub's dialect has no negated class; `[!` is a literal `!`.
		b.WriteString(regexp.QuoteMeta("!"))
		i++
	}
	closed := false
	for ; i < len(rs); i++ {
		c := rs[i]
		if c == ']' {
			closed = true
			break
		}
		if c == '-' {
			b.WriteString("-")
			continue
		}
		b.WriteString(regexp.QuoteMeta(string(c)))
	}
	if !closed {
		return "", 0, fmt.Errorf("filter pattern %q has an unterminated [ character class", pattern)
	}
	b.WriteString("]")
	return b.String(), i, nil
}

// GlobSet is an ordered list of patterns evaluated together.
type GlobSet []*Glob

// CompileGlobs compiles a filter list, reporting the first bad pattern.
func CompileGlobs(patterns []string) (GlobSet, error) {
	out := make(GlobSet, 0, len(patterns))
	for _, p := range patterns {
		g, err := CompileGlob(p)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

// Matches reports whether s is selected by the set.
//
// Order matters and the last matching pattern wins, which is how GitHub lets a
// `!` pattern carve an exception out of a broad one and a later positive
// pattern put part of it back. A set containing only negative patterns selects
// everything they do not exclude.
func (gs GlobSet) Matches(s string) bool {
	if len(gs) == 0 {
		return true
	}
	allNegative := true
	for _, g := range gs {
		if !g.neg {
			allNegative = false
			break
		}
	}
	selected := allNegative
	for _, g := range gs {
		if g.Match(s) {
			selected = !g.neg
		}
	}
	return selected
}

// MatchesAny reports whether any of the candidates is selected, which is the
// rule for `paths:`: a run happens when at least one changed file matches.
func (gs GlobSet) MatchesAny(candidates []string) bool {
	for _, c := range candidates {
		if gs.Matches(c) {
			return true
		}
	}
	return false
}
