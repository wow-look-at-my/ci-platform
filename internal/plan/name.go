package plan

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// MaxJobNameLength is GitHub's cap on a generated job name. Longer names are
// truncated with an ellipsis, and branch protection matches the truncated form,
// so the cap is part of the contract rather than cosmetic.
const MaxJobNameLength = 100

// RenderValue renders one matrix value as text. Scalars stringify; a list or
// object renders as compact JSON, which is only used for the leg identity --
// display names flatten containers instead (see NameSegments).
func RenderValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case int32:
		return strconv.FormatInt(int64(t), 10)
	case int64:
		return strconv.FormatInt(t, 10)
	case uint64:
		return strconv.FormatUint(t, 10)
	case float32:
		return strconv.FormatFloat(float64(t), 'g', -1, 32)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case fmt.Stringer:
		return t.String()
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// NameSegments flattens one matrix value into display-name segments the way
// GitHub does: it walks the value and emits every scalar leaf, so an object
// value contributes its leaves rather than a rendering of the object. An empty
// string contributes nothing.
//
// Object keys are walked in sorted order. GitHub walks them in source order,
// which a map in the IR cannot preserve.
func NameSegments(v any) []string {
	var out []string
	appendLeaves(&out, v)
	return out
}

func appendLeaves(out *[]string, v any) {
	if list, ok := asList(v); ok {
		for _, e := range list {
			appendLeaves(out, e)
		}
		return
	}
	if obj, ok := asObject(v); ok {
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			appendLeaves(out, obj[k])
		}
		return
	}
	if s := RenderValue(v); s != "" {
		*out = append(*out, s)
	}
}

// DisplayName is the check run name, which branch protection matches on, so it
// must be byte-identical to GitHub's.
//
//   - no matrix: the evaluated `name:`, else the job key.
//   - matrix, no `name:`: "<key> (<v1>, <v2>)" over the declared dimensions.
//   - matrix with `name:`: the evaluated name verbatim. GitHub evaluates the
//     name after strategy expansion and it replaces the generated name, so no
//     suffix is appended.
func DisplayName(key string, name model.Expr, leg *Leg, ev Evaluator) (string, error) {
	if !name.Empty() {
		s, err := EvalString(ev, name)
		if err != nil {
			return "", fmt.Errorf("job %q name: %w", key, err)
		}
		if s == "" {
			return "", fmt.Errorf("job %q name evaluated to an empty string", key)
		}
		return s, nil
	}
	if leg == nil {
		return key, nil
	}
	suffix := leg.Suffix()
	if suffix == "" {
		return key, nil
	}
	return truncateName(key + " " + suffix), nil
}

func truncateName(s string) string {
	if len(s) <= MaxJobNameLength {
		return s
	}
	return s[:MaxJobNameLength-3] + "..."
}
