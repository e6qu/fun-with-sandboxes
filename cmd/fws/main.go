// Command fws is the engine's command-line companion. fws catalog lists
// the darwin images fws builds and publishes: their names, tags, where
// they are published and what each holds.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/e6qu/fun-with-sandboxes/internal/catalog"
)

const usage = `usage: fws catalog [-json] [-published] [KIND|NAME...]

Lists the catalog's images, one line a name: its tags, kind and summary.
-published prints where each tag is published instead; -json prints the
catalog as JSON. Arguments keep the images of those kinds or names.
`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "fws:", err)
		os.Exit(2)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "catalog" {
		return fmt.Errorf("%s", strings.TrimSpace(usage))
	}
	fs := flag.NewFlagSet("catalog", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "print the catalog as JSON")
	published := fs.Bool("published", false, "print where each tag is published")
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("%v\n%s", err, usage)
	}
	images := pick(fs.Args())
	if len(images) == 0 {
		return fmt.Errorf("no image of kind or name %s", strings.Join(fs.Args(), ", "))
	}
	switch {
	case *asJSON:
		e := json.NewEncoder(out)
		e.SetIndent("", "  ")
		return e.Encode(images)
	case *published:
		for _, i := range images {
			for _, t := range i.Tags {
				fmt.Fprintf(out, "%s\t%s\n", i.Ref(t), i.Published(t))
			}
		}
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "IMAGE\tTAGS\tKIND\tSUMMARY")
	for _, i := range images {
		fmt.Fprintf(w, "fws/%s\t%s\t%s\t%s\n", i.Name, strings.Join(i.Tags, ", "), i.Kind, i.Summary)
	}
	return w.Flush()
}

// pick keeps the images of the kinds or names filters names, all of them
// without filters.
func pick(filters []string) []catalog.Image {
	if len(filters) == 0 {
		return catalog.Images
	}
	var out []catalog.Image
	for _, i := range catalog.Images {
		for _, f := range filters {
			f = strings.TrimPrefix(f, "fws/")
			if f == i.Kind || f == i.Name {
				out = append(out, i)
				break
			}
		}
	}
	return out
}
