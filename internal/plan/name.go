package plan

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// RenderValue renders one matrix value the way GitHub Actions renders it in a
// job name. Scalars stringify; a list or object renders as compact JSON, which
// is the only stable rendering available for a value with no text form.
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

// DisplayName is the check run name, which branch protection matches on, so it
// must be byte-identical to GHA's.
//
//   - no matrix: the evaluated `name:`, else the job key.
//   - matrix, no `name:`: "<key> (<v1>, <v2>)" in declaration order.
//   - matrix with `name:`: the evaluated name verbatim; GHA appends no suffix
//     because the author is expected to interpolate matrix values themselves.
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
	if leg == nil || len(leg.Order) == 0 {
		return key, nil
	}
	return key + " " + leg.Suffix(), nil
}
