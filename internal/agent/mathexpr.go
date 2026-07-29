package agent

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// evaluateExpression parses and evaluates a basic arithmetic expression
// supporting +, -, *, /, ^, parentheses, decimals, and unary minus.
//
// It's a small hand-rolled recursive-descent parser rather than a library
// dependency, deliberately — students can read the whole thing in one
// sitting: expr -> term -> factor -> power -> number.
func evaluateExpression(expr string) (float64, error) {
	p := &mathParser{input: []rune(strings.TrimSpace(expr))}
	val, err := p.parseExpr()
	if err != nil {
		return 0, err
	}
	p.skipSpace()
	if p.pos < len(p.input) {
		return 0, fmt.Errorf("unexpected character %q at position %d", p.input[p.pos], p.pos)
	}
	return val, nil
}

type mathParser struct {
	input []rune
	pos   int
}

func (p *mathParser) skipSpace() {
	for p.pos < len(p.input) && unicode.IsSpace(p.input[p.pos]) {
		p.pos++
	}
}

func (p *mathParser) peek() rune {
	p.skipSpace()
	if p.pos >= len(p.input) {
		return 0
	}
	return p.input[p.pos]
}

// expr := term (('+' | '-') term)*
func (p *mathParser) parseExpr() (float64, error) {
	val, err := p.parseTerm()
	if err != nil {
		return 0, err
	}
	for {
		c := p.peek()
		if c != '+' && c != '-' {
			break
		}
		p.pos++
		rhs, err := p.parseTerm()
		if err != nil {
			return 0, err
		}
		if c == '+' {
			val += rhs
		} else {
			val -= rhs
		}
	}
	return val, nil
}

// term := factor (('*' | '/') factor)*
func (p *mathParser) parseTerm() (float64, error) {
	val, err := p.parseFactor()
	if err != nil {
		return 0, err
	}
	for {
		c := p.peek()
		if c != '*' && c != '/' {
			break
		}
		p.pos++
		rhs, err := p.parseFactor()
		if err != nil {
			return 0, err
		}
		if c == '*' {
			val *= rhs
		} else {
			if rhs == 0 {
				return 0, fmt.Errorf("division by zero")
			}
			val /= rhs
		}
	}
	return val, nil
}

// factor := power ('^' factor)?   (right-associative)
func (p *mathParser) parseFactor() (float64, error) {
	base, err := p.parsePower()
	if err != nil {
		return 0, err
	}
	if p.peek() == '^' {
		p.pos++
		exp, err := p.parseFactor()
		if err != nil {
			return 0, err
		}
		return integerPow(base, exp), nil
	}
	return base, nil
}

// power := '-' power | '(' expr ')' | number
func (p *mathParser) parsePower() (float64, error) {
	c := p.peek()
	if c == '-' {
		p.pos++
		val, err := p.parsePower()
		return -val, err
	}
	if c == '(' {
		p.pos++
		val, err := p.parseExpr()
		if err != nil {
			return 0, err
		}
		if p.peek() != ')' {
			return 0, fmt.Errorf("expected ')' at position %d", p.pos)
		}
		p.pos++
		return val, nil
	}
	return p.parseNumber()
}

func (p *mathParser) parseNumber() (float64, error) {
	p.skipSpace()
	start := p.pos
	for p.pos < len(p.input) && (unicode.IsDigit(p.input[p.pos]) || p.input[p.pos] == '.') {
		p.pos++
	}
	if start == p.pos {
		return 0, fmt.Errorf("expected number at position %d", p.pos)
	}
	return strconv.ParseFloat(string(p.input[start:p.pos]), 64)
}

// integerPow supports integer exponents, which is plenty for a teaching demo.
func integerPow(base, exp float64) float64 {
	n := int(exp)
	neg := n < 0
	if neg {
		n = -n
	}
	result := 1.0
	for i := 0; i < n; i++ {
		result *= base
	}
	if neg {
		return 1 / result
	}
	return result
}
