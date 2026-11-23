package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/e6qu/fun-with-sandboxes/internal/catalog"
)

func TestCatalog(t *testing.T) {
	var b bytes.Buffer
	if err := run([]string{"catalog"}, &b); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.String(), "IMAGE") || !strings.Contains(b.String(), "fws/python") || strings.Count(b.String(), "\n") != len(catalog.Images)+1 {
		t.Errorf("catalog:\n%s", b.String())
	}
	b.Reset()
	if err := run([]string{"catalog", "-published", "lambda"}, &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "fws/lambda/python:3.14\tghcr.io/e6qu/fws-lambda/python:3.14\n") || strings.Contains(b.String(), "postgres") {
		t.Errorf("published lambda:\n%s", b.String())
	}
	b.Reset()
	if err := run([]string{"catalog", "-json", "fws/node"}, &b); err != nil {
		t.Fatal(err)
	}
	var got []catalog.Image
	if json.Unmarshal(b.Bytes(), &got) != nil || len(got) != 1 || got[0].Name != "node" {
		t.Errorf("json node:\n%s", b.String())
	}
	if err := run([]string{"catalog", "nothing"}, &b); err == nil {
		t.Error("an unknown filter listed images")
	}
	if err := run(nil, &b); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("no command: %v", err)
	}
}
