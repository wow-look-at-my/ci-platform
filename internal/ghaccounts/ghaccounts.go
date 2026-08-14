// Package ghaccounts is a named set of GitHub account logins.
//
// Two questions in this platform have the same shape and the same failure
// mode: which accounts may run work here (internal/installpolicy) and which
// accounts may administer it (internal/adminauth). Both are answered by an
// operator-supplied list of logins, both are case-insensitive because GitHub
// logins are, and both are catastrophic if an empty value is read as "allow
// everybody". Writing that once means it cannot drift into two answers.
package ghaccounts

import (
	"fmt"
	"sort"
	"strings"
)

// Anyone is the explicit opt-out. It has to be spelled out, alone, because
// "everybody" is a decision with a blast radius and must not be reachable by
// leaving a variable empty.
const Anyone = "*"

// Set is a set of account logins.
type Set struct {
	logins map[string]struct{}
	order  []string
	any    bool
	name   string
}

// New builds a set. name is what the set is for, used in error messages so a
// startup failure names the variable the operator has to fix.
//
// An empty list is an error, not an open or a closed door: both readings are
// defensible, which is exactly why the operator has to say which one they mean.
func New(name string, logins []string) (*Set, error) {
	s := &Set{logins: map[string]struct{}{}, name: name}
	for _, raw := range logins {
		login := strings.TrimSpace(raw)
		if login == "" {
			continue
		}
		if login == Anyone {
			s.any = true
			continue
		}
		if strings.ContainsAny(login, "/@ ") {
			return nil, fmt.Errorf("%s: %q is not an account login; this is a list of "+
				"users and organisations, not repositories or email addresses", name, login)
		}
		key := strings.ToLower(login)
		if _, dup := s.logins[key]; dup {
			continue
		}
		s.logins[key] = struct{}{}
		s.order = append(s.order, login)
	}
	if s.any && len(s.order) > 0 {
		return nil, fmt.Errorf("%s: %q allows every account, so listing %s alongside it states two "+
			"different intentions; remove one", name, Anyone, strings.Join(s.order, ", "))
	}
	if !s.any && len(s.order) == 0 {
		return nil, fmt.Errorf("%s: no accounts listed; name the accounts, or %q for anybody",
			name, Anyone)
	}
	sort.Strings(s.order)
	return s, nil
}

// Contains reports whether a login is in the set. GitHub logins are
// case-insensitive, so the comparison is too.
func (s *Set) Contains(login string) bool {
	if s.any {
		return true
	}
	_, ok := s.logins[strings.ToLower(strings.TrimSpace(login))]
	return ok
}

// IsAnyone reports whether the set is the open one.
func (s *Set) IsAnyone() bool { return s.any }

// Logins lists the members, sorted.
func (s *Set) Logins() []string { return append([]string(nil), s.order...) }

// String describes the set for a startup log line.
func (s *Set) String() string {
	if s.any {
		return "any account (" + Anyone + ")"
	}
	return strings.Join(s.order, ", ")
}

// Parse splits a comma or whitespace separated list, which is how a set arrives
// from the environment.
func Parse(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
}
