package dockerfile

import (
	"fmt"
	"strings"
)

// Expand substitutes $VAR, ${VAR}, ${VAR:-default} and ${VAR:+alternative}
// from lookup, keeping \$ as a literal dollar, as Dockerfile instructions
// other than RUN, CMD and ENTRYPOINT do.
func Expand(s string, lookup func(string) (string, bool)) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) && s[i+1] == '$' {
			b.WriteByte('$')
			i++
			continue
		}
		if c != '$' || i+1 == len(s) {
			b.WriteByte(c)
			continue
		}
		if s[i+1] == '{' {
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return "", fmt.Errorf("missing '}' in %q", s)
			}
			expr := s[i+2 : i+end]
			i += end
			name, mod, word := expr, "", ""
			for _, op := range []string{":-", ":+"} {
				if k := strings.Index(expr, op); k >= 0 {
					name, mod, word = expr[:k], op, expr[k+2:]
					break
				}
			}
			if !validName(name) {
				return "", fmt.Errorf("bad substitution %q", "${"+expr+"}")
			}
			v, ok := lookup(name)
			switch mod {
			case ":-":
				if !ok || v == "" {
					v = word
				}
			case ":+":
				if ok && v != "" {
					v = word
				} else {
					v = ""
				}
			}
			b.WriteString(v)
			continue
		}
		j := i + 1
		for j < len(s) && (isAlnum(s[j]) || s[j] == '_') {
			j++
		}
		if j == i+1 {
			b.WriteByte('$')
			continue
		}
		v, _ := lookup(s[i+1 : j])
		b.WriteString(v)
		i = j - 1
	}
	return b.String(), nil
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func validName(n string) bool {
	if n == "" {
		return false
	}
	for i := 0; i < len(n); i++ {
		if !isAlnum(n[i]) && n[i] != '_' {
			return false
		}
	}
	return true
}
