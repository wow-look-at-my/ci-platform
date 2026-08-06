package expr

import (
	"fmt"
	"strings"
)

// StatusFunctions are the four functions whose presence in an `if:` changes
// what the condition means when a dependency has failed.
var StatusFunctions = []string{"success", "failure", "cancelled", "always"}

// ReferencesStatusFunction reports whether an if: condition calls one of the
// status functions.
//
// The scheduler needs this because GitHub compiles a condition naming none of
// them to `success() && (<cond>)`, so a plain `if: github.ref == 'x'` does not
// run after a failed dependency. Answering it by scanning the raw text would be
// fooled by a string literal such as `if: contains(msg, 'success()')`, which is
// why this walks the parsed AST.
func ReferencesStatusFunction(raw string) (bool, error) {
	src := strings.TrimSpace(raw)
	if src == "" {
		return false, nil
	}
	// An `if:` may be a bare expression or one or more ${{ }} interpolations.
	if !strings.Contains(src, "${{") {
		n, err := parse(src)
		if err != nil {
			return false, err
		}
		return walkForStatusCall(n), nil
	}
	rest := src
	for {
		i := strings.Index(rest, "${{")
		if i < 0 {
			return false, nil
		}
		body, after, ok := splitExpr(rest[i+3:])
		if !ok {
			return false, fmt.Errorf("unterminated expression: %q", rest[i:])
		}
		n, err := parse(body)
		if err != nil {
			return false, err
		}
		if walkForStatusCall(n) {
			return true, nil
		}
		rest = after
	}
}

func walkForStatusCall(n node) bool {
	// The parser returns nodes by value; accept pointers too so the walk does
	// not silently miss a branch if that ever changes.
	switch t := n.(type) {
	case *callNode:
		return walkForStatusCall(*t)
	case callNode:
		for _, name := range StatusFunctions {
			if strings.EqualFold(t.name, name) {
				return true
			}
		}
		for _, a := range t.args {
			if walkForStatusCall(a) {
				return true
			}
		}
	case *propNode:
		return walkForStatusCall(*t)
	case propNode:
		return walkForStatusCall(t.x)
	case *indexNode:
		return walkForStatusCall(*t)
	case indexNode:
		return walkForStatusCall(t.x) || walkForStatusCall(t.i)
	case *unaryNode:
		return walkForStatusCall(*t)
	case unaryNode:
		return walkForStatusCall(t.x)
	case *binNode:
		return walkForStatusCall(*t)
	case binNode:
		return walkForStatusCall(t.l) || walkForStatusCall(t.r)
	}
	return false
}
