package engine

import (
	"encoding/json"
	"strings"
)

// Filters holds a Docker API `filters` query parameter: a map from filter name
// to accepted values.
type Filters map[string][]string

// ParseFilters accepts both encodings the Docker CLI has used:
// {"label":{"a=b":true}} and {"label":["a=b"]}.
func ParseFilters(raw string) (Filters, error) {
	f := Filters{}
	if raw == "" {
		return f, nil
	}
	var asMap map[string]map[string]bool
	if err := json.Unmarshal([]byte(raw), &asMap); err == nil {
		for k, vs := range asMap {
			for v, on := range vs {
				if on {
					f[k] = append(f[k], v)
				}
			}
		}
		return f, nil
	}
	var asList map[string][]string
	if err := json.Unmarshal([]byte(raw), &asList); err != nil {
		return nil, invalid("invalid filter: %v", err)
	}
	return Filters(asList), nil
}

func (f Filters) has(key string) bool { return len(f[key]) > 0 }

// matchName reports whether value contains any of the key's values; an absent
// key matches everything.
func (f Filters) matchName(key, value string) bool {
	if !f.has(key) {
		return true
	}
	for _, want := range f[key] {
		if strings.Contains(value, strings.TrimPrefix(want, "/")) {
			return true
		}
	}
	return false
}

func (f Filters) matchExact(key string, values ...string) bool {
	if !f.has(key) {
		return true
	}
	for _, want := range f[key] {
		for _, v := range values {
			if v == want {
				return true
			}
		}
	}
	return false
}

// matchLabels requires every `label` filter to hold and every `label!` filter
// to fail.
func (f Filters) matchLabels(labels map[string]string) bool {
	for _, want := range f["label"] {
		if !labelMatch(labels, want) {
			return false
		}
	}
	for _, want := range f["label!"] {
		if labelMatch(labels, want) {
			return false
		}
	}
	return true
}

func labelMatch(labels map[string]string, want string) bool {
	k, v, hasValue := strings.Cut(want, "=")
	got, ok := labels[k]
	if !ok {
		return false
	}
	return !hasValue || got == v
}

func (f Filters) boolValue(key string) (bool, bool) {
	if !f.has(key) {
		return false, false
	}
	v := strings.ToLower(f[key][0])
	return v == "true" || v == "1", true
}
