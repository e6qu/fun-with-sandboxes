package catalog

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The catalog lists every image docs/IMAGES.md's table documents, at its tags,
// and no other.
func TestMatchesTheREADME(t *testing.T) {
	b, err := os.ReadFile("../../docs/IMAGES.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	start := strings.Index(s, "| Image |")
	end := strings.Index(s[start:], "\n\n")
	if start < 0 || end < 0 {
		t.Fatal("docs/IMAGES.md has no image table")
	}
	var documented []string
	for _, line := range strings.Split(s[start:start+end], "\n") {
		cell, _, _ := strings.Cut(strings.TrimPrefix(line, "| "), " |")
		documented = append(documented, regexp.MustCompile("`fws/([^`]+)`").FindAllString(cell, -1)...)
	}
	var listed []string
	for _, i := range Images {
		for _, tag := range i.Tags {
			listed = append(listed, "`"+i.Ref(tag)+"`")
		}
	}
	sort.Strings(documented)
	sort.Strings(listed)
	if strings.Join(documented, " ") != strings.Join(listed, " ") {
		t.Errorf("docs/IMAGES.md documents\n%v\nthe catalog lists\n%v", documented, listed)
	}
}

func TestPublished(t *testing.T) {
	i := Image{Name: "lambda/python"}
	if got := i.Published("3.14"); got != "ghcr.io/e6qu/fws-lambda/python:3.14" {
		t.Errorf("Published = %s", got)
	}
}
