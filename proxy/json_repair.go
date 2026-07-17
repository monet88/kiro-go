package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// repairStructuredToolJSON applies only the deterministic repairs allowed by
// the ADR for Structured Tool Event input. It never invents fields/defaults.
// Returns a parsed object or an error. Raw input is never included in errors.
func repairStructuredToolJSON(raw string) (map[string]interface{}, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]interface{}{}, nil
	}
	if obj, err := parseJSONObject(raw); err == nil {
		return obj, nil
	}
	repaired, err := applyDeterministicJSONRepairs(raw)
	if err != nil {
		return nil, err
	}
	obj, err := parseJSONObject(repaired)
	if err != nil {
		return nil, fmt.Errorf("tool input is not valid JSON object after repair")
	}
	return obj, nil
}

func parseJSONObject(raw string) (map[string]interface{}, error) {
	var v interface{}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	// Reject any non-whitespace trailing bytes (Decoder.More only detects another value).
	rest := strings.TrimSpace(raw[int(dec.InputOffset()):])
	if rest != "" {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	obj, ok := v.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("tool input must be a JSON object")
	}
	return obj, nil
}

// applyDeterministicJSONRepairs implements the closed repair set:
// - escape raw newline/CR/tab inside strings
// - remove trailing commas before } or ]
// - close unterminated string
// - close unmatched object/array containers when balance is positive
// Rejects negative balance or otherwise invalid tokens.
func applyDeterministicJSONRepairs(raw string) (string, error) {
	var b strings.Builder
	b.Grow(len(raw) + 8)

	inString := false
	escape := false
	var stack []byte // '{', '['

	i := 0
	for i < len(raw) {
		r, size := utf8.DecodeRuneInString(raw[i:])
		if r == utf8.RuneError && size == 1 {
			return "", fmt.Errorf("invalid UTF-8 in tool input")
		}
		ch := raw[i]

		if inString {
			if escape {
				b.WriteByte(ch)
				escape = false
				i++
				continue
			}
			switch ch {
			case '\\':
				b.WriteByte(ch)
				escape = true
			case '"':
				b.WriteByte(ch)
				inString = false
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				b.WriteByte(ch)
			}
			i++
			continue
		}

		// Outside strings.
		switch ch {
		case '"':
			inString = true
			b.WriteByte(ch)
		case '{', '[':
			stack = append(stack, ch)
			b.WriteByte(ch)
		case '}', ']':
			if len(stack) == 0 {
				return "", fmt.Errorf("negative container balance")
			}
			open := stack[len(stack)-1]
			if (ch == '}' && open != '{') || (ch == ']' && open != '[') {
				return "", fmt.Errorf("mismatched container")
			}
			stack = stack[:len(stack)-1]
			// Drop a trailing comma immediately before this closer in output.
			trimTrailingComma(&b)
			b.WriteByte(ch)
		case ',':
			b.WriteByte(ch)
		default:
			// Allow whitespace and tokens; structural validity checked by parse.
			b.WriteByte(ch)
		}
		i++
	}

	if inString {
		// Close unterminated string.
		if escape {
			// Dangling backslash: drop it rather than invent an escape.
			s := b.String()
			if strings.HasSuffix(s, `\`) {
				b.Reset()
				b.WriteString(s[:len(s)-1])
			}
		}
		b.WriteByte('"')
		inString = false
	}

	// Close unmatched containers when balance is positive.
	for len(stack) > 0 {
		open := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		trimTrailingComma(&b)
		if open == '{' {
			b.WriteByte('}')
		} else {
			b.WriteByte(']')
		}
	}
	return b.String(), nil
}

func trimTrailingComma(b *strings.Builder) {
	s := b.String()
	// Trim spaces then one comma.
	i := len(s) - 1
	for i >= 0 && (s[i] == ' ' || s[i] == '\n' || s[i] == '\r' || s[i] == '\t') {
		i--
	}
	if i >= 0 && s[i] == ',' {
		b.Reset()
		b.WriteString(s[:i])
		// re-append trailing spaces that followed the comma? drop them.
	}
}
