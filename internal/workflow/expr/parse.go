package expr

import "fmt"

type node interface{ isNode() }

type litNode struct{ v any }
type nameNode struct{ name string }

// propNode is `x.name`. name "*" is the wildcard/array filter.
type propNode struct {
	x    node
	name string
}

// indexNode is `x[i]`. A nil i is `x[*]`.
type indexNode struct{ x, i node }

type callNode struct {
	name string
	args []node
}

type unaryNode struct {
	op string
	x  node
}

type binNode struct {
	op   string
	l, r node
}

func (litNode) isNode()   {}
func (nameNode) isNode()  {}
func (propNode) isNode()  {}
func (indexNode) isNode() {}
func (callNode) isNode()  {}
func (unaryNode) isNode() {}
func (binNode) isNode()   {}

type parser struct {
	toks []token
	i    int
}

func parse(src string) (node, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	if p.peek().kind == tokEOF {
		return nil, fmt.Errorf("invalid expression: empty expression")
	}
	n, err := p.parseBinary(0)
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tokEOF {
		return nil, fmt.Errorf("invalid expression: unexpected %s at offset %d", describe(t), t.pos)
	}
	return n, nil
}

func (p *parser) peek() token    { return p.toks[p.i] }
func (p *parser) advance() token { t := p.toks[p.i]; p.i++; return t }

func (p *parser) isOp(s string) bool {
	t := p.peek()
	return t.kind == tokOp && t.text == s
}

func (p *parser) expectOp(s string) error {
	if !p.isOp(s) {
		t := p.peek()
		return fmt.Errorf("invalid expression: expected %q but found %s at offset %d", s, describe(t), t.pos)
	}
	p.i++
	return nil
}

// precedence, loosest first. Unary ! and the postfix operators bind tighter
// than anything here and are handled structurally.
func precOf(t token) int {
	if t.kind != tokOp {
		return 0
	}
	switch t.text {
	case "||":
		return 1
	case "&&":
		return 2
	case "==", "!=", "<", "<=", ">", ">=":
		return 3
	}
	return 0
}

func (p *parser) parseBinary(min int) (node, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		prec := precOf(t)
		if prec == 0 || prec <= min {
			return left, nil
		}
		p.advance()
		right, err := p.parseBinary(prec)
		if err != nil {
			return nil, err
		}
		left = binNode{op: t.text, l: left, r: right}
	}
}

func (p *parser) parseUnary() (node, error) {
	if p.isOp("!") {
		p.advance()
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return unaryNode{op: "!", x: x}, nil
	}
	return p.parsePostfix()
}

func (p *parser) parsePostfix() (node, error) {
	x, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.isOp("."):
			p.advance()
			t := p.advance()
			switch {
			case t.kind == tokIdent, t.kind == tokBool, t.kind == tokNull:
				x = propNode{x: x, name: t.text}
			case t.kind == tokOp && t.text == "*":
				x = propNode{x: x, name: "*"}
			default:
				return nil, fmt.Errorf("invalid expression: expected a property name after '.' but found %s at offset %d", describe(t), t.pos)
			}
		case p.isOp("["):
			p.advance()
			if p.isOp("*") {
				p.advance()
				if err := p.expectOp("]"); err != nil {
					return nil, err
				}
				x = indexNode{x: x}
				continue
			}
			i, err := p.parseBinary(0)
			if err != nil {
				return nil, err
			}
			if err := p.expectOp("]"); err != nil {
				return nil, err
			}
			x = indexNode{x: x, i: i}
		default:
			return x, nil
		}
	}
}

func (p *parser) parsePrimary() (node, error) {
	t := p.advance()
	switch t.kind {
	case tokNumber:
		return litNode{v: t.num}, nil
	case tokString:
		return litNode{v: t.str}, nil
	case tokBool:
		return litNode{v: t.num == 1}, nil
	case tokNull:
		return litNode{v: nil}, nil
	case tokIdent:
		if p.isOp("(") {
			return p.parseCall(t.text)
		}
		return nameNode{name: t.text}, nil
	case tokOp:
		if t.text == "(" {
			n, err := p.parseBinary(0)
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return n, nil
		}
	}
	return nil, fmt.Errorf("invalid expression: unexpected %s at offset %d", describe(t), t.pos)
}

func (p *parser) parseCall(name string) (node, error) {
	p.advance() // "("
	c := callNode{name: name}
	if p.isOp(")") {
		p.advance()
		return c, nil
	}
	for {
		a, err := p.parseBinary(0)
		if err != nil {
			return nil, err
		}
		c.args = append(c.args, a)
		if p.isOp(",") {
			p.advance()
			continue
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		return c, nil
	}
}

func describe(t token) string {
	switch t.kind {
	case tokEOF:
		return "end of expression"
	case tokString:
		return fmt.Sprintf("string %s", t.text)
	default:
		return fmt.Sprintf("%q", t.text)
	}
}
