package engine

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
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

// matchName reports whether any of the key's values, as regular
// expressions, matches one of values, as Docker's name filters match; an
// absent key matches everything.
func (f Filters) matchName(key string, values ...string) bool {
	if !f.has(key) {
		return true
	}
	for _, want := range f[key] {
		re, err := regexp.Compile(want)
		for _, v := range values {
			if v == want || err == nil && re.MatchString(v) {
				return true
			}
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

// until reports whether created is before every `until` filter, each a
// timestamp or a duration back from now, as Docker's prune filters take it.
func (f Filters) until(created time.Time) (bool, error) {
	for _, v := range f["until"] {
		t, err := parseTimestamp(v, time.Now())
		if err != nil {
			return false, invalid("invalid until filter %q: %v", v, err)
		}
		if !created.Before(t) {
			return false, nil
		}
	}
	return true, nil
}

// parseTimestamp reads a time as Docker's API filters accept it: a Go
// duration back from now, an RFC 3339 date or date-time, or Unix seconds
// with an optional fraction.
func parseTimestamp(v string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(v); err == nil {
		return now.Add(-d), nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return t, nil
		}
	}
	secs, frac, _ := strings.Cut(v, ".")
	s, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	var ns int64
	if frac != "" {
		frac = (frac + "000000000")[:9]
		if ns, err = strconv.ParseInt(frac, 10, 64); err != nil {
			return time.Time{}, err
		}
	}
	return time.Unix(s, ns), nil
}
