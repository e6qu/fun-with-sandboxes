package dockerfile

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	src := `# escape=\
# a comment
FROM fws/toolbox:1 AS base

from --platform=darwin/arm64 base
ARG GREETING=hello
ENV A=1 B="two words"
RUN ["/toolbox", "put", "/x", "y"]
RUN echo one \
  # an interleaved comment
  two
COPY --from=base --chmod=0755 a b /dst/
CMD ["serve"]
`
	ins, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	cmds := []string{}
	for _, i := range ins {
		cmds = append(cmds, i.Cmd)
	}
	if want := []string{"FROM", "FROM", "ARG", "ENV", "RUN", "RUN", "COPY", "CMD"}; !reflect.DeepEqual(cmds, want) {
		t.Fatalf("instructions %v, want %v", cmds, want)
	}
	if ins[1].Flags["platform"] != "darwin/arm64" || ins[1].Raw != "base" {
		t.Errorf("FROM flags %v raw %q", ins[1].Flags, ins[1].Raw)
	}
	if !ins[4].JSON || !reflect.DeepEqual(ins[4].Args, []string{"/toolbox", "put", "/x", "y"}) {
		t.Errorf("exec-form RUN: %+v", ins[4])
	}
	if ins[5].JSON || ins[5].Args[0] != "echo one   two" {
		t.Errorf("continued RUN: %q", ins[5].Args)
	}
	if ins[5].Line != 9 {
		t.Errorf("continued RUN line %d, want 9", ins[5].Line)
	}
	if ins[6].Flags["from"] != "base" || ins[6].Flags["chmod"] != "0755" || ins[6].Raw != "a b /dst/" {
		t.Errorf("COPY: %+v", ins[6])
	}
}

func TestParseBacktickEscape(t *testing.T) {
	ins, err := Parse(strings.NewReader("# escape=`\nFROM scratch\nCOPY a `\n  b\n"))
	if err != nil {
		t.Fatal(err)
	}
	if ins[1].Raw != "a   b" {
		t.Fatalf("COPY raw %q", ins[1].Raw)
	}
}

func TestParseErrors(t *testing.T) {
	for _, src := range []string{"", "# only a comment\n", "FROM x\nRUN <<EOF\necho\nEOF\n", "# escape=x\nFROM y\n"} {
		if _, err := Parse(strings.NewReader(src)); err == nil {
			t.Errorf("Parse(%q) accepted", src)
		}
	}
}

func TestWordsAndKeyValues(t *testing.T) {
	w, err := Words(`a "b c" 'd e' f\ g`)
	if err != nil || !reflect.DeepEqual(w, []string{"a", "b c", "d e", "f g"}) {
		t.Fatalf("Words = %q, %v", w, err)
	}
	kv, err := KeyValues(`A=1 B="two words" C=`, true)
	if err != nil || !reflect.DeepEqual(kv, [][2]string{{"A", "1"}, {"B", "two words"}, {"C", ""}}) {
		t.Fatalf("KeyValues = %q, %v", kv, err)
	}
	kv, err = KeyValues(`PATH /usr/bin:/bin`, true)
	if err != nil || !reflect.DeepEqual(kv, [][2]string{{"PATH", "/usr/bin:/bin"}}) {
		t.Fatalf("legacy KeyValues = %q, %v", kv, err)
	}
	if _, err := KeyValues(`novalue`, false); err == nil {
		t.Error("LABEL without = accepted")
	}
}

func TestExpand(t *testing.T) {
	env := map[string]string{"A": "x", "EMPTY": ""}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	for in, want := range map[string]string{
		"$A/${A}":           "x/x",
		"${MISSING:-def}":   "def",
		"${EMPTY:-def}":     "def",
		"${A:-def}":         "x",
		"${A:+alt}":         "alt",
		"${MISSING:+alt}":   "",
		`\$A`:               "$A",
		"cost $5 $":         "cost  $",
		"$A_suffix ${A}_ok": " x_ok",
	} {
		got, err := Expand(in, lookup)
		if err != nil || got != want {
			t.Errorf("Expand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"${A", "${}", "${A B}"} {
		if _, err := Expand(bad, lookup); err == nil {
			t.Errorf("Expand(%q) accepted", bad)
		}
	}
}
