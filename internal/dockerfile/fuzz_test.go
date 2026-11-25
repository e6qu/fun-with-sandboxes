package dockerfile

import (
	"strings"
	"testing"
)

// The parser reads Dockerfiles a build is handed, so it MUST NOT crash on any input.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"FROM fws/base:1\nRUN echo hi\n",
		"# syntax=docker/dockerfile:1\nFROM a AS b\nCOPY --from=b /x /y\n",
		"ARG V=1\nFROM x:${V}\nENV A=\"b c\" D=e\nLABEL k=v\nCMD [\"a\", \"b\"]\n",
		"RUN a \\\n  b \\\n\n  c\n",
		"RUN <<EOF\necho\nEOF\n",
		"ONBUILD RUN x\nHEALTHCHECK --interval=1s CMD [\"true\"]\nSHELL [\"/bin/sh\", \"-c\"]\n",
		"\\\n", "ENV\n", "FROM\t\n", "COPY [\"a\"\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		Parse(strings.NewReader(s))
	})
}

// Variable expansion runs on every instruction's arguments.
func FuzzExpand(f *testing.F) {
	for _, s := range []string{"$A", "${A}", "${A:-d}", "${A:+x}", "${B:-${A}}", "\\$A", "$", "${", "${A", "${A:", "${:-x}", "'$A'"} {
		f.Add(s)
	}
	vars := map[string]string{"A": "a", "B": ""}
	f.Fuzz(func(t *testing.T, s string) {
		Expand(s, func(k string) (string, bool) { v, ok := vars[k]; return v, ok })
	})
}

// Words and KeyValues split instructions' arguments as a shell would.
func FuzzWords(f *testing.F) {
	for _, s := range []string{`a b`, `"a b" c`, `'a\' b'`, `a\ b`, `"unterminated`, `k=v k2="v 2"`, `k v`, `=`, `"\`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		Words(s)
		KeyValues(s, false)
		KeyValues(s, true)
	})
}
