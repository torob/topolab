package config

import (
	"fmt"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

type environment interface {
	LookupEnv(string) (string, bool)
}

type lookupFunc func(string) (string, bool)

func (f lookupFunc) LookupEnv(key string) (string, bool) { return f(key) }

func interpolateDocument(root *yaml.Node, env environment) ([]string, error) {
	var warnings []string
	seenScalars := map[*yaml.Node]bool{}
	var walk func(*yaml.Node, bool) error
	walk = func(n *yaml.Node, isKey bool) error {
		if n.Kind == yaml.AliasNode {
			return walk(n.Alias, isKey)
		}
		if n.Kind == yaml.MappingNode {
			for i := 0; i < len(n.Content); i += 2 {
				if err := walk(n.Content[i], true); err != nil {
					return err
				}
				if err := walk(n.Content[i+1], false); err != nil {
					return err
				}
			}
			return nil
		}
		if n.Kind == yaml.SequenceNode || n.Kind == yaml.DocumentNode {
			for _, c := range n.Content {
				if err := walk(c, false); err != nil {
					return err
				}
			}
			return nil
		}
		if isKey || n.Kind != yaml.ScalarNode || n.Tag != "!!str" || n.Style == yaml.SingleQuotedStyle {
			return nil
		}
		if seenScalars[n] {
			return nil
		}
		seenScalars[n] = true
		value, ws, err := interpolateString(n.Value, env)
		if err != nil {
			return fmt.Errorf("line %d: %w", n.Line, err)
		}
		n.Value = value
		warnings = append(warnings, ws...)
		return nil
	}
	if err := walk(root, false); err != nil {
		return nil, err
	}
	return warnings, nil
}

func interpolateString(input string, env environment) (string, []string, error) {
	var out strings.Builder
	var warnings []string
	for i := 0; i < len(input); {
		if input[i] != '$' {
			out.WriteByte(input[i])
			i++
			continue
		}
		if i+1 >= len(input) {
			out.WriteByte('$')
			i++
			continue
		}
		if input[i+1] == '$' {
			out.WriteByte('$')
			i += 2
			continue
		}
		if input[i+1] == '{' {
			end, err := matchingBrace(input, i+2)
			if err != nil {
				return "", nil, err
			}
			value, ws, err := expandBraced(input[i+2:end], env)
			if err != nil {
				return "", nil, err
			}
			out.WriteString(value)
			warnings = append(warnings, ws...)
			i = end + 1
			continue
		}
		if isVarStart(rune(input[i+1])) {
			j := i + 2
			for j < len(input) && isVarContinue(rune(input[j])) {
				j++
			}
			name := input[i+1 : j]
			value, ok := env.LookupEnv(name)
			if !ok {
				warnings = append(warnings, fmt.Sprintf("variable %s is unset; substituting an empty string", name))
			}
			out.WriteString(value)
			i = j
			continue
		}
		out.WriteByte('$')
		i++
	}
	return out.String(), warnings, nil
}

func matchingBrace(s string, start int) (int, error) {
	depth := 1
	for i := start; i < len(s); i++ {
		switch {
		case s[i] == '$' && i+1 < len(s) && s[i+1] == '$':
			i++
		case s[i] == '$' && i+1 < len(s) && s[i+1] == '{':
			depth++
			i++
		case s[i] == '}':
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return 0, fmt.Errorf("unterminated interpolation expression")
}

func expandBraced(expr string, env environment) (string, []string, error) {
	if expr == "" || !isVarStart(rune(expr[0])) {
		return "", nil, fmt.Errorf("invalid interpolation expression ${%s}", expr)
	}
	i := 1
	for i < len(expr) && isVarContinue(rune(expr[i])) {
		i++
	}
	name := expr[:i]
	op := ""
	rest := ""
	if i < len(expr) {
		for _, candidate := range []string{":-", ":?", ":+", "-", "?", "+"} {
			if strings.HasPrefix(expr[i:], candidate) {
				op = candidate
				rest = expr[i+len(candidate):]
				break
			}
		}
		if op == "" {
			return "", nil, fmt.Errorf("unsupported interpolation expression ${%s}", expr)
		}
	}
	value, set := env.LookupEnv(name)
	nonempty := set && value != ""
	expandRest := func() (string, []string, error) { return interpolateString(rest, env) }
	switch op {
	case "":
		if !set {
			return "", []string{fmt.Sprintf("variable %s is unset; substituting an empty string", name)}, nil
		}
		return value, nil, nil
	case ":-":
		if !nonempty {
			return expandRest()
		}
	case "-":
		if !set {
			return expandRest()
		}
	case ":?":
		if !nonempty {
			message, ws, err := expandRest()
			if err != nil {
				return "", nil, err
			}
			if message == "" {
				message = name + " is required"
			}
			return "", ws, fmt.Errorf("%s", message)
		}
	case "?":
		if !set {
			message, ws, err := expandRest()
			if err != nil {
				return "", nil, err
			}
			if message == "" {
				message = name + " is required"
			}
			return "", ws, fmt.Errorf("%s", message)
		}
	case ":+":
		if nonempty {
			return expandRest()
		}
		return "", nil, nil
	case "+":
		if set {
			return expandRest()
		}
		return "", nil, nil
	}
	return value, nil, nil
}

func isVarStart(r rune) bool    { return r == '_' || unicode.IsLetter(r) && r <= unicode.MaxASCII }
func isVarContinue(r rune) bool { return isVarStart(r) || r >= '0' && r <= '9' }
