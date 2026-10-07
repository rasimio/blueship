package handler

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math/big"
	"strconv"
	"strings"
)

// Calculation markers are a report-template operation, never executable code
// or a tool call. Exact rational arithmetic avoids binary float rounding of
// prices. Source grounding still has to establish the operands and units.
func renderGraphCalculations(report string) (string, error) {
	const prefix = "{{calc:"
	var out strings.Builder
	for count := 0; ; count++ {
		start := strings.Index(report, prefix)
		if start < 0 {
			out.WriteString(report)
			return out.String(), nil
		}
		if count >= 128 {
			return "", fmt.Errorf("too many calculation markers")
		}
		out.WriteString(report[:start])
		rest := report[start+len(prefix):]
		end := strings.Index(rest, "}}")
		if end < 0 || end > 512 {
			return "", fmt.Errorf("unclosed or oversized calculation marker")
		}
		expression, places, ok := strings.Cut(rest[:end], ";")
		precision, err := strconv.Atoi(strings.TrimSpace(places))
		if !ok || err != nil || precision < 0 || precision > 6 {
			return "", fmt.Errorf("calculation needs precision from 0 to 6")
		}
		expression = strings.TrimSpace(expression)
		if strings.Contains(expression, "//") || strings.Contains(expression, "/*") {
			return "", fmt.Errorf("calculation comments are not supported")
		}
		for _, c := range expression {
			if !strings.ContainsRune("0123456789.+-*/() \t", c) {
				return "", fmt.Errorf("calculation accepts only decimal arithmetic")
			}
		}
		node, err := parser.ParseExpr(expression)
		if err != nil {
			return "", fmt.Errorf("invalid calculation expression")
		}
		remaining := 128
		value, err := graphCalculationValue(node, &remaining)
		if err != nil {
			return "", err
		}
		out.WriteString(value.FloatString(precision))
		report = rest[end+2:]
	}
}

func graphCalculationValue(node ast.Expr, remaining *int) (*big.Rat, error) {
	*remaining--
	if *remaining < 0 {
		return nil, fmt.Errorf("calculation too complex")
	}
	var value *big.Rat
	switch n := node.(type) {
	case *ast.BasicLit:
		if n.Kind != token.INT && n.Kind != token.FLOAT {
			return nil, fmt.Errorf("calculation requires decimal operands")
		}
		// No base prefixes or exponent notation; parser already validates syntax.
		var ok bool
		value, ok = new(big.Rat).SetString(n.Value)
		if n.Kind == token.INT {
			integer, valid := new(big.Int).SetString(n.Value, 10)
			ok = valid
			if valid {
				value = new(big.Rat).SetInt(integer)
			}
		}
		if !ok {
			return nil, fmt.Errorf("invalid decimal operand")
		}
	case *ast.ParenExpr:
		return graphCalculationValue(n.X, remaining)
	case *ast.UnaryExpr:
		v, err := graphCalculationValue(n.X, remaining)
		if err != nil {
			return nil, err
		}
		switch n.Op {
		case token.ADD:
			value = v
		case token.SUB:
			value = new(big.Rat).Neg(v)
		default:
			return nil, fmt.Errorf("unsupported calculation operator")
		}
	case *ast.BinaryExpr:
		left, err := graphCalculationValue(n.X, remaining)
		if err != nil {
			return nil, err
		}
		right, err := graphCalculationValue(n.Y, remaining)
		if err != nil {
			return nil, err
		}
		value = new(big.Rat)
		switch n.Op {
		case token.ADD:
			value.Add(left, right)
		case token.SUB:
			value.Sub(left, right)
		case token.MUL:
			value.Mul(left, right)
		case token.QUO:
			if right.Sign() == 0 {
				return nil, fmt.Errorf("calculation divides by zero")
			}
			value.Quo(left, right)
		default:
			return nil, fmt.Errorf("unsupported calculation operator")
		}
	default:
		return nil, fmt.Errorf("unsupported calculation expression")
	}
	if value.Num().BitLen() > 8192 || value.Denom().BitLen() > 8192 {
		return nil, fmt.Errorf("calculation exceeds numeric limit")
	}
	return value, nil
}
