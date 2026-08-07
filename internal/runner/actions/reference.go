// Package actions resolves `uses:` references to a directory on disk and parses
// the action.yml that describes how to run them.
package actions

import (
	"fmt"
	"strings"
)

// Kind distinguishes the three reference forms.
type Kind string

const (
	// KindRepo is owner/repo@ref or owner/repo/path@ref.
	KindRepo Kind = "repo"
	// KindLocal is ./path, resolved inside the checked-out workspace.
	KindLocal Kind = "local"
	// KindDocker is docker://image[:tag].
	KindDocker Kind = "docker"
)

// Reference is a parsed `uses:` value.
type Reference struct {
	Kind  Kind
	Owner string
	Repo  string
	// Path is the sub-directory inside the repo holding action.yml, "" at root.
	Path string
	Ref  string
	// LocalPath is the workspace-relative directory for KindLocal.
	LocalPath string
	// Image is the full image reference for KindDocker.
	Image string
	// Text is the reference exactly as written, for error messages.
	Text string
}

// String renders the reference as written.
func (r Reference) String() string { return r.Text }

// CacheKey is the content-addressed cache key, valid only once Ref names an
// immutable sha.
func (r Reference) CacheKey(sha string) string {
	return fmt.Sprintf("%s/%s@%s", r.Owner, r.Repo, sha)
}

// ParseReference parses a `uses:` value. A value it cannot parse is an error
// naming the value: an unparseable reference is a config failure, never a step
// that quietly does nothing.
func ParseReference(s string) (Reference, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Reference{}, fmt.Errorf("uses: is empty")
	}
	ref := Reference{Text: raw}

	if strings.HasPrefix(raw, "docker://") {
		ref.Kind = KindDocker
		ref.Image = strings.TrimPrefix(raw, "docker://")
		if ref.Image == "" {
			return Reference{}, fmt.Errorf("uses: %q names no image", raw)
		}
		return ref, nil
	}
	if strings.HasPrefix(raw, "./") || strings.HasPrefix(raw, ".\\") || raw == "." {
		ref.Kind = KindLocal
		ref.LocalPath = strings.TrimPrefix(strings.TrimPrefix(raw, "./"), ".\\")
		if strings.Contains(ref.LocalPath, "..") {
			return Reference{}, fmt.Errorf("uses: %q escapes the workspace", raw)
		}
		return ref, nil
	}
	if strings.HasPrefix(raw, "/") {
		return Reference{}, fmt.Errorf("uses: %q is an absolute path; local actions must start with ./", raw)
	}

	body, gitRef, ok := strings.Cut(raw, "@")
	if !ok || strings.TrimSpace(gitRef) == "" {
		return Reference{}, fmt.Errorf("uses: %q has no @ref; a repository action must pin a ref", raw)
	}
	parts := strings.Split(body, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return Reference{}, fmt.Errorf("uses: %q is not owner/repo[/path]@ref", raw)
	}
	// Every element is checked, not just the sub-path: Owner and Repo become
	// directory names in the action cache, and Path is joined onto the
	// extracted directory before it is copied into the sandbox. A "." or ".."
	// in any of them walks out of the cache and into the runner host.
	for _, part := range parts {
		if part == "." || part == ".." {
			return Reference{}, fmt.Errorf("uses: %q contains a path traversal", raw)
		}
	}
	ref.Kind = KindRepo
	ref.Owner, ref.Repo = parts[0], parts[1]
	ref.Path = strings.Join(parts[2:], "/")
	ref.Ref = strings.TrimSpace(gitRef)
	return ref, nil
}
