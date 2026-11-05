package main

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	for _, c := range []struct {
		argv          []string
		verbose, zero bool
		wait          int
		args          []string
	}{
		{[]string{"-vz", "-w", "1", "localhost", "6379"}, true, true, 1, []string{"localhost", "6379"}},
		{[]string{"-z", "-w1", "-v", "127.0.0.1", "80"}, true, true, 1, []string{"127.0.0.1", "80"}},
		{[]string{"-zw", "3", "localhost", "80"}, false, true, 3, []string{"localhost", "80"}},
		{[]string{"-z", "--", "localhost", "80"}, false, true, 0, []string{"localhost", "80"}},
	} {
		v, z, w, args, err := parse(c.argv)
		if err != nil || v != c.verbose || z != c.zero || w != c.wait || !reflect.DeepEqual(args, c.args) {
			t.Errorf("parse(%q) = %v %v %d %q %v", c.argv, v, z, w, args, err)
		}
	}
	for _, argv := range [][]string{{"-x", "h", "1"}, {"-w"}, {"-w", "soon", "h", "1"}} {
		if _, _, _, _, err := parse(argv); err == nil {
			t.Errorf("parse(%q) accepted it", argv)
		}
	}
}
