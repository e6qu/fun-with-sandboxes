package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The engine's lock is taken before a container's, never after: a function
// that holds a container's lock calls no engine method that takes the
// engine's, as Create, holding the engine's, reads its containers' states
// under theirs, and the two would wait on each other for good.
func TestNoEngineLockUnderAContainers(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	funcRE := regexp.MustCompile(`(?s)\nfunc ([^\n]*)\{\n(.*?)\n\}\n`)
	takerRE := regexp.MustCompile(`^\(e \*Engine\) (\w+)\(`)
	src := map[string]string{}
	takers := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src[f] = string(b)
		for _, m := range funcRE.FindAllStringSubmatch(string(b), -1) {
			if n := takerRE.FindStringSubmatch(m[1]); n != nil && strings.Contains(m[2], "e.mu.Lock()") {
				takers[n[1]] = true
			}
		}
	}
	lockRE := regexp.MustCompile(`\b(c|v|o|other|member|m2)\.mu\.Lock\(\)`)
	for f, s := range src {
		for _, m := range funcRE.FindAllStringSubmatch(s, -1) {
			body := m[2]
			for _, loc := range lockRE.FindAllStringSubmatchIndex(body, -1) {
				v := body[loc[2]:loc[3]]
				rest := body[loc[1]:]
				held := rest
				if i := strings.Index(rest, v+".mu.Unlock()"); i >= 0 && !strings.HasPrefix(strings.TrimSpace(rest), "defer "+v+".mu.Unlock()") {
					held = rest[:i]
				}
				for name := range takers {
					if regexp.MustCompile(`\be\.` + name + `\(`).MatchString(held) {
						t.Errorf("%s: func %s calls e.%s, which takes the engine's lock, under %s's", f, strings.TrimSpace(m[1]), name, v)
					}
				}
			}
		}
	}
}
