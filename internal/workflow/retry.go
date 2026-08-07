package workflow

import (
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"gopkg.in/yaml.v3"
)

var (
	retryKeys    = []string{"attempts", "on", "backoff", "initial", "max", "jitter"}
	failureClass = []model.FailureClass{model.ClassUser, model.ClassInfra, model.ClassConfig}
	backoffKinds = []model.BackoffKind{
		model.BackoffNone, model.BackoffFixed, model.BackoffLinear, model.BackoffExponential,
	}
)

// retry parses this platform's own `retry:` block. Unset fields inherit
// model.DefaultRetryPolicy(), so `retry: {attempts: 5}` means "the default
// policy, but five attempts".
func (p *parser) retry(n *yaml.Node, where string) (*model.RetryPolicy, error) {
	pol := model.DefaultRetryPolicy()
	var sawAttempts bool
	err := p.each(n, where, retryKeys, func(key string, kn, vn *yaml.Node) error {
		at := where + "." + key
		switch key {
		case "attempts":
			a, err := p.integer(vn, at)
			if err != nil {
				return err
			}
			if a < 1 {
				return p.errf(vn, "%s must be at least 1, found %d", at, a)
			}
			pol.Attempts, sawAttempts = a, true
			return nil
		case "on":
			names, err := p.stringList(vn, at)
			if err != nil {
				return err
			}
			pol.On = nil
			for _, name := range names {
				c := model.FailureClass(name)
				if !containsClass(failureClass, c) {
					return p.errf(vn, "%s must list only user, infra or config, found %q", at, name)
				}
				pol.On = append(pol.On, c)
			}
			return nil
		case "backoff":
			s, err := p.nonEmpty(vn, at)
			if err != nil {
				return err
			}
			k := model.BackoffKind(s)
			if !containsBackoff(backoffKinds, k) {
				return p.errf(vn, "%s must be none, fixed, linear or exponential, found %q", at, s)
			}
			pol.Backoff = k
			return nil
		case "initial", "max":
			d, err := p.duration(vn, at)
			if err != nil {
				return err
			}
			if key == "initial" {
				pol.Initial = d
			} else {
				pol.Max = d
			}
			return nil
		case "jitter":
			j, err := p.boolean(vn, at)
			if err != nil {
				return err
			}
			pol.Jitter = j
			return nil
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !sawAttempts {
		return nil, p.errf(n, "%s must set `attempts:`", where)
	}
	if pol.Max > 0 && pol.Initial > pol.Max {
		return nil, p.errf(n, "%s.initial (%s) is longer than %s.max (%s)", where, pol.Initial, where, pol.Max)
	}
	return &pol, nil
}

// duration requires a unit: a bare number would silently mean seconds to one
// reader and minutes to another.
func (p *parser) duration(n *yaml.Node, where string) (time.Duration, error) {
	s, err := p.nonEmpty(n, where)
	if err != nil {
		return 0, err
	}
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0, p.errf(n, "%s must be a duration with a unit such as 5s or 2m, found %q", where, s)
	}
	if d < 0 {
		return 0, p.errf(n, "%s must not be negative, found %q", where, s)
	}
	return d, nil
}

func containsClass(hay []model.FailureClass, needle model.FailureClass) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func containsBackoff(hay []model.BackoffKind, needle model.BackoffKind) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
