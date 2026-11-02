package logfile

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLineWriterSplitsLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	notified := 0
	a.Notify = func() { notified++ }
	w := &LineWriter{Stream: "stdout", To: a}
	w.Write([]byte("one\ntw"))
	w.Write([]byte("o\nthree"))
	w.Flush()
	a.Close()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var got []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		var e Entry
		if err := json.Unmarshal(s.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if e.Stream != "stdout" || e.Time.IsZero() {
			t.Fatalf("entry %+v", e)
		}
		got = append(got, e.Log)
	}
	if len(got) != 3 || got[0] != "one\n" || got[1] != "two\n" || got[2] != "three" || notified != 3 {
		t.Fatalf("entries %q, %d notifications", got, notified)
	}
}
