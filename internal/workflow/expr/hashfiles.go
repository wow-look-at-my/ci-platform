package expr

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// hashFiles hashes every file matching the patterns, relative to the workspace
// root: sha256 of the concatenated sha256 digests of the matched files, in
// sorted path order. A pattern starting with '!' excludes.
//
// With no filesystem configured this is an error, never a plausible-looking
// digest: a fake hash would silently corrupt every cache key derived from it.
func (e *Evaluator) hashFiles(patterns []string) (string, error) {
	if e.fsys == nil {
		return "", fmt.Errorf("expression error: hashFiles(%s) needs a workspace filesystem, and none was configured for this evaluation", strings.Join(quoteAll(patterns), ", "))
	}
	root := path.Clean(e.root)
	if root == "" || root == "." || root == "/" {
		root = "."
	}

	var include, exclude []string
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			return "", fmt.Errorf("expression error: hashFiles received an empty pattern")
		}
		if strings.HasPrefix(p, "!") {
			exclude = append(exclude, path.Clean(p[1:]))
			continue
		}
		include = append(include, path.Clean(p))
	}
	if len(include) == 0 {
		return "", fmt.Errorf("expression error: hashFiles was given only exclusion patterns")
	}

	var matches []string
	err := fs.WalkDir(e.fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := p
		if root != "." {
			rel = strings.TrimPrefix(strings.TrimPrefix(p, root), "/")
		}
		if !matchAny(include, rel) || matchAny(exclude, rel) {
			return nil
		}
		matches = append(matches, p)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("expression error: hashFiles could not walk %q: %w", root, err)
	}
	// GHA returns an empty string when nothing matched; the caller decides
	// whether that is acceptable for its cache key.
	if len(matches) == 0 {
		return "", nil
	}
	sort.Strings(matches)

	overall := sha256.New()
	for _, m := range matches {
		h, err := e.hashOne(m)
		if err != nil {
			return "", err
		}
		overall.Write(h)
	}
	return hex.EncodeToString(overall.Sum(nil)), nil
}

func (e *Evaluator) hashOne(p string) ([]byte, error) {
	f, err := e.fsys.Open(p)
	if err != nil {
		return nil, fmt.Errorf("expression error: hashFiles could not read %q: %w", p, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, fmt.Errorf("expression error: hashFiles could not read %q: %w", p, err)
	}
	return h.Sum(nil), nil
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if globMatch(p, name) {
			return true
		}
	}
	return false
}

// globMatch matches a slash-separated path against a pattern supporting `*`
// and `?` within a segment and `**` across segments.
func globMatch(pattern, name string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegs(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// ** matches zero or more path segments.
			for i := 0; i <= len(name); i++ {
				if matchSegs(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 || !matchSeg(pat[0], name[0]) {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// matchSeg matches one path segment; `*` matches any run of characters within
// the segment and `?` matches exactly one.
func matchSeg(pat, s string) bool {
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		switch {
		case pi < len(pat) && (pat[pi] == '?' || pat[pi] == s[si]):
			pi++
			si++
		case pi < len(pat) && pat[pi] == '*':
			star, mark = pi, si
			pi++
		case star >= 0:
			mark++
			pi, si = star+1, mark
		default:
			return false
		}
	}
	for pi < len(pat) && pat[pi] == '*' {
		pi++
	}
	return pi == len(pat)
}
