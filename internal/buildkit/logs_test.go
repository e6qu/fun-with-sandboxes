package buildkit

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

// The content store's fsverity warning is dropped where fsverity is not
// supported; its other warnings, and every other line, are written.
func TestQuietFormatterDropsOnlyUnsupportedFsverity(t *testing.T) {
	var out bytes.Buffer
	l := logrus.New()
	l.SetOutput(&out)
	l.SetFormatter(quietFormatter{&logrus.TextFormatter{DisableTimestamp: true}})
	l.WithError(errors.New("fsverity is only supported on Linux systems")).Warn("failed check for fsverity support")
	l.WithError(errors.New("permission denied")).Warn("failed check for fsverity support")
	l.Warn("another warning")
	got := out.String()
	if strings.Contains(got, "only supported on Linux") {
		t.Errorf("the unsupported fsverity warning was written: %q", got)
	}
	if !strings.Contains(got, "permission denied") || !strings.Contains(got, "another warning") {
		t.Errorf("other warnings were dropped: %q", got)
	}
}
