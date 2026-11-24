// Package dockerfile parses Dockerfiles into instructions and expands their
// variables, following the Dockerfile reference.
package dockerfile

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
)

// Instruction is one Dockerfile instruction.
type Instruction struct {
	// Cmd is the instruction keyword in upper case.
	Cmd string
	// Flags are the leading --name=value options, such as COPY --from.
	Flags map[string]string
	// Args holds the JSON (exec) form's elements, or the single shell-form
	// string.
	Args []string
	// JSON is set for the exec form.
	JSON bool
	// Raw is the text after the keyword and flags.
	Raw  string
	Line int
}

// knownDirectives are the parser directives the Dockerfile reference
// defines.
var knownDirectives = map[string]bool{"syntax": true, "escape": true, "check": true}

var directive = regexp.MustCompile(`^#\s*([a-zA-Z][a-zA-Z0-9]*)\s*=\s*(.+?)\s*$`)

// Parse reads a Dockerfile.
func Parse(r io.Reader) ([]Instruction, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	escape := '\\'
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	i := 0
	// Parser directives sit at the very top, before any other line. An
	// unknown one is a comment, after which no directive is read.
	for ; i < len(lines); i++ {
		m := directive.FindStringSubmatch(lines[i])
		if m == nil || !knownDirectives[strings.ToLower(m[1])] {
			break
		}
		if strings.EqualFold(m[1], "escape") {
			if m[2] != "\\" && m[2] != "`" {
				return nil, fmt.Errorf("line %d: invalid escape token %q", i+1, m[2])
			}
			escape = rune(m[2][0])
		}
	}

	var out []Instruction
	for i < len(lines) {
		start := i + 1
		line := lines[i]
		i++
		if isBlankOrComment(line) {
			continue
		}
		logical := strings.TrimLeftFunc(line, unicode.IsSpace)
		for continues(logical, escape) {
			logical = strings.TrimRightFunc(logical, unicode.IsSpace)
			logical = logical[:len(logical)-1]
			for i < len(lines) && isBlankOrComment(lines[i]) {
				i++
			}
			if i == len(lines) {
				break
			}
			logical += lines[i]
			i++
		}
		ins, err := parseInstruction(logical, start)
		if err != nil {
			return nil, err
		}
		out = append(out, ins)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the Dockerfile has no instructions")
	}
	return out, nil
}

func isBlankOrComment(l string) bool {
	t := strings.TrimSpace(l)
	return t == "" || strings.HasPrefix(t, "#")
}

func continues(l string, escape rune) bool {
	t := strings.TrimRightFunc(l, unicode.IsSpace)
	return strings.HasSuffix(t, string(escape))
}

// flagsOf are the options each instruction takes in the classic builder;
// any other is refused, as dockerd's classic builder refuses it, rather
// than dropped.
var flagsOf = map[string]map[string]bool{
	"FROM":        {"platform": true},
	"COPY":        {"from": true, "chown": true, "chmod": true},
	"ADD":         {"chown": true, "chmod": true},
	"RUN":         {},
	"HEALTHCHECK": {"interval": true, "timeout": true, "start-period": true, "start-interval": true, "retries": true},
}

// buildKitFlags are RUN's options that only BuildKit implements.
var buildKitFlags = map[string]bool{"mount": true, "network": true, "security": true}

func parseInstruction(l string, line int) (Instruction, error) {
	word, rest, _ := strings.Cut(l, " ")
	if i := strings.IndexFunc(word, unicode.IsSpace); i >= 0 {
		word, rest = word[:i], word[i:]+" "+rest
	}
	ins := Instruction{Cmd: strings.ToUpper(word), Flags: map[string]string{}, Line: line}
	rest = strings.TrimSpace(rest)
	if strings.Contains(rest, "<<") && (ins.Cmd == "RUN" || ins.Cmd == "COPY") && heredoc.MatchString(rest) {
		return ins, fmt.Errorf("line %d: heredocs are not supported", line)
	}
	if allowed, ok := flagsOf[ins.Cmd]; ok {
		for strings.HasPrefix(rest, "--") {
			tok, after, _ := strings.Cut(rest, " ")
			k, v, ok := strings.Cut(strings.TrimPrefix(tok, "--"), "=")
			if !ok {
				v = "true"
			}
			switch {
			case ins.Cmd == "RUN" && buildKitFlags[k]:
				return ins, fmt.Errorf("line %d: the --%s option requires BuildKit", line, k)
			case !allowed[k]:
				return ins, fmt.Errorf("line %d: unknown flag: %s", line, k)
			}
			if _, dup := ins.Flags[k]; dup {
				return ins, fmt.Errorf("line %d: Duplicate flag specified: %s", line, k)
			}
			ins.Flags[k] = v
			rest = strings.TrimSpace(after)
		}
	}
	ins.Raw = rest
	if strings.HasPrefix(rest, "[") {
		var arr []string
		if err := json.Unmarshal([]byte(rest), &arr); err == nil {
			ins.Args, ins.JSON = arr, true
			return ins, nil
		}
	}
	if rest != "" {
		ins.Args = []string{rest}
	}
	return ins, nil
}

var heredoc = regexp.MustCompile(`<<-?["']?[A-Za-z_]`)

// Words splits a shell-form argument list on unquoted whitespace, removing
// quotes and escapes, as COPY, ADD, EXPOSE, VOLUME and the like read theirs.
func Words(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	in := false
	var quote rune
	esc := false
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
			in = true
		case r == '\\' && quote != '\'':
			esc = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
			in = true
		case unicode.IsSpace(r):
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote in %q", s)
	}
	if in {
		out = append(out, cur.String())
	}
	return out, nil
}

// KeyValues parses ENV and LABEL arguments: `k=v k2="v 2"`, or the legacy
// single-pair `k v...` form when legacy is allowed.
func KeyValues(s string, legacy bool) ([][2]string, error) {
	words, err := Words(s)
	if err != nil {
		return nil, err
	}
	if len(words) == 0 {
		return nil, fmt.Errorf("missing arguments")
	}
	if legacy && !strings.Contains(words[0], "=") {
		k, v, _ := strings.Cut(strings.TrimSpace(s), " ")
		return [][2]string{{k, strings.TrimSpace(v)}}, nil
	}
	var out [][2]string
	for _, w := range words {
		k, v, ok := strings.Cut(w, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("%q is not of the form key=value", w)
		}
		out = append(out, [2]string{k, v})
	}
	return out, nil
}
