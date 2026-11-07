// Command ub is the utility belt Confluent's images run from their
// /etc/confluent/docker scripts, for the commands those scripts use:
//
//	ub ensure NAME                 fails unless the environment sets NAME
//	ub path PATH OPERATION         fails unless PATH exists, or is readable,
//	                               writable or executable
//	ub render-properties SPEC      prints the properties the environment
//	                               holds, as the JSON file SPEC selects them
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"

	"github.com/e6qu/fun-with-sandboxes/internal/sandboxenv"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ub ensure NAME | path PATH OPERATION | render-properties SPEC")
	}
	switch cmd, args := args[0], args[1:]; {
	case cmd == "ensure" && len(args) == 1:
		if os.Getenv(args[0]) == "" {
			return fmt.Errorf("environment variable %s is required", args[0])
		}
		return nil
	case cmd == "path" && len(args) == 2:
		return checkPath(sandboxenv.Path(args[0]), args[0], args[1])
	case cmd == "render-properties" && len(args) == 1:
		b, err := os.ReadFile(sandboxenv.Path(args[0]))
		if err != nil {
			return err
		}
		var spec Spec
		if err := json.Unmarshal(b, &spec); err != nil {
			return fmt.Errorf("%s: %w", args[0], err)
		}
		_, err = os.Stdout.WriteString(Render(spec, os.Environ()))
		return err
	}
	return fmt.Errorf("ub: unknown command or wrong arguments: %s", strings.Join(args, " "))
}

func checkPath(host, name, op string) error {
	var mode uint32
	switch op {
	case "existence":
	case "readable":
		mode = 4
	case "writable":
		mode = 2
	case "executable":
		mode = 1
	default:
		return fmt.Errorf("ub path: unknown operation %q", op)
	}
	if _, err := os.Stat(host); err != nil {
		return fmt.Errorf("%s does not exist", name)
	}
	if mode != 0 && syscall.Access(host, mode) != nil {
		return fmt.Errorf("%s is not %s", name, op)
	}
	return nil
}

// Spec selects and names the properties of a component, as Confluent's
// *-propertiesSpec.json files do.
type Spec struct {
	// Prefixes are the variable prefixes the properties come from, each
	// with whether the property keeps it.
	Prefixes          map[string]bool   `json:"prefixes"`
	Excludes          []string          `json:"excludes"`
	Renamed           map[string]string `json:"renamed"`
	Defaults          map[string]string `json:"defaults"`
	ExcludeWithPrefix string            `json:"excludeWithPrefix"`
}

// Render prints the properties that environ's variables set over the
// spec's defaults, one key=value a line, sorted by key.
func Render(spec Spec, environ []string) string {
	props := map[string]string{}
	for k, v := range spec.Defaults {
		props[k] = v
	}
	excluded := map[string]bool{}
	for _, e := range spec.Excludes {
		excluded[e] = true
	}
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		if excluded[name] || spec.ExcludeWithPrefix != "" && strings.HasPrefix(name, spec.ExcludeWithPrefix) {
			continue
		}
		if key, ok := spec.Renamed[name]; ok {
			props[key] = value
			continue
		}
		for prefix, keep := range spec.Prefixes {
			rest, ok := strings.CutPrefix(name, prefix+"_")
			if !ok || rest == "" {
				continue
			}
			if keep {
				rest = name
			}
			props[PropertyName(rest)] = value
			break
		}
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, props[k])
	}
	return b.String()
}

// PropertyName turns a variable's name into a property's: lower case, with
// "___" a dash, "__" an underscore and "_" a dot.
func PropertyName(name string) string {
	var b strings.Builder
	name = strings.ToLower(name)
	for i := 0; i < len(name); {
		switch {
		case strings.HasPrefix(name[i:], "___"):
			b.WriteByte('-')
			i += 3
		case strings.HasPrefix(name[i:], "__"):
			b.WriteByte('_')
			i += 2
		case name[i] == '_':
			b.WriteByte('.')
			i++
		default:
			b.WriteByte(name[i])
			i++
		}
	}
	return b.String()
}
