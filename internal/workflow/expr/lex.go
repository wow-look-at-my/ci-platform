package expr

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

type tokKind int

const (
	tokEOF tokKind = iota
	tokIdent
	tokNumber
	tokString
	tokBool
	tokNull
	tokOp
)

type token struct {
	kind tokKind
	// text is the source text: the identifier, the keyword, or the operator.
	text string
	num  float64
	str  string
	pos  int
}

type lexer struct {
	src string
	i   int
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// lex tokenizes the whole expression up front; expressions are short and the
// parser needs one token of lookahead in several places.
func lex(src string) ([]token, error) {
	l := &lexer{src: src}
	var out []token
	for {
		t, err := l.next()
		if err != nil {
			return nil, err
		}
		out = append(out, t)
		if t.kind == tokEOF {
			return out, nil
		}
	}
}

func (l *lexer) next() (token, error) {
	for l.i < len(l.src) && isSpace(l.src[l.i]) {
		l.i++
	}
	if l.i >= len(l.src) {
		return token{kind: tokEOF, pos: l.i}, nil
	}
	start := l.i
	c := l.src[l.i]

	switch {
	case isIdentStart(c):
		return l.lexIdent(), nil
	case isDigit(c):
		return l.lexNumber()
	case c == '-' && l.i+1 < len(l.src) && (isDigit(l.src[l.i+1]) || l.src[l.i+1] == '.'):
		return l.lexNumber()
	case c == '.' && l.i+1 < len(l.src) && isDigit(l.src[l.i+1]):
		return l.lexNumber()
	case c == '\'':
		return l.lexString()
	}

	two := ""
	if l.i+1 < len(l.src) {
		two = l.src[l.i : l.i+2]
	}
	switch two {
	case "||", "&&", "==", "!=", "<=", ">=":
		l.i += 2
		return token{kind: tokOp, text: two, pos: start}, nil
	}
	switch c {
	case '(', ')', '[', ']', '.', ',', '*', '<', '>', '!':
		l.i++
		return token{kind: tokOp, text: string(c), pos: start}, nil
	case '|', '&', '=':
		return token{}, fmt.Errorf("invalid expression: unexpected %q at offset %d (did you mean %q?)", string(c), start, string(c)+string(c))
	}
	return token{}, fmt.Errorf("invalid expression: unexpected character %q at offset %d", string(c), start)
}

func (l *lexer) lexIdent() token {
	start := l.i
	l.i++
	for l.i < len(l.src) {
		c := l.src[l.i]
		if isIdentPart(c) {
			l.i++
			continue
		}
		// A hyphen belongs to the identifier only when more identifier
		// follows: property names like `my-step` are legal, a trailing one is
		// not part of the name.
		if c == '-' && l.i+1 < len(l.src) && isIdentPart(l.src[l.i+1]) {
			l.i += 2
			continue
		}
		break
	}
	text := l.src[start:l.i]
	// Keyword matching is case-SENSITIVE (LexicalAnalyzer.cs uses Ordinal), so
	// `TRUE` is a named-value, not a boolean. Function names are not: those are
	// resolved case-insensitively at call time.
	switch text {
	case "true":
		return token{kind: tokBool, text: text, num: 1, pos: start}
	case "false":
		return token{kind: tokBool, text: text, pos: start}
	case "null":
		return token{kind: tokNull, text: text, pos: start}
	case "NaN":
		return token{kind: tokNumber, text: text, num: math.NaN(), pos: start}
	case "Infinity":
		return token{kind: tokNumber, text: text, num: math.Inf(1), pos: start}
	}
	return token{kind: tokIdent, text: text, pos: start}
}

func (l *lexer) lexNumber() (token, error) {
	start := l.i
	if l.src[l.i] == '-' {
		l.i++
	}
	if strings.HasPrefix(strings.ToLower(l.src[l.i:]), "0x") {
		l.i += 2
		h := l.i
		for l.i < len(l.src) && isHex(l.src[l.i]) {
			l.i++
		}
		if l.i == h {
			return token{}, fmt.Errorf("invalid expression: malformed hex number at offset %d", start)
		}
		n, err := strconv.ParseUint(l.src[h:l.i], 16, 64)
		if err != nil {
			return token{}, fmt.Errorf("invalid expression: %q is not a valid number", l.src[start:l.i])
		}
		v := float64(n)
		if l.src[start] == '-' {
			v = -v
		}
		return token{kind: tokNumber, text: l.src[start:l.i], num: v, pos: start}, nil
	}
	for l.i < len(l.src) && isDigit(l.src[l.i]) {
		l.i++
	}
	if l.i < len(l.src) && l.src[l.i] == '.' {
		l.i++
		for l.i < len(l.src) && isDigit(l.src[l.i]) {
			l.i++
		}
	}
	if l.i < len(l.src) && (l.src[l.i] == 'e' || l.src[l.i] == 'E') {
		j := l.i + 1
		if j < len(l.src) && (l.src[j] == '+' || l.src[j] == '-') {
			j++
		}
		if j < len(l.src) && isDigit(l.src[j]) {
			l.i = j
			for l.i < len(l.src) && isDigit(l.src[l.i]) {
				l.i++
			}
		}
	}
	text := l.src[start:l.i]
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return token{}, fmt.Errorf("invalid expression: %q is not a valid number", text)
	}
	return token{kind: tokNumber, text: text, num: v, pos: start}, nil
}

func isHex(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func (l *lexer) lexString() (token, error) {
	start := l.i
	l.i++ // opening quote
	var b strings.Builder
	for l.i < len(l.src) {
		c := l.src[l.i]
		if c != '\'' {
			b.WriteByte(c)
			l.i++
			continue
		}
		if l.i+1 < len(l.src) && l.src[l.i+1] == '\'' {
			b.WriteByte('\'')
			l.i += 2
			continue
		}
		l.i++
		return token{kind: tokString, str: b.String(), text: l.src[start:l.i], pos: start}, nil
	}
	return token{}, fmt.Errorf("invalid expression: unterminated string starting at offset %d", start)
}
