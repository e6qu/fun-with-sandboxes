package buildkit

import (
	"strings"

	"github.com/sirupsen/logrus"
)

// containerd's local content store probes fsverity, a Linux feature, each
// time a store opens, and warns that the probe failed: on darwin it always
// does, and the store works without it. Every BuildKit fws starts, a
// nested engine's included, opens one, so the warning says nothing but
// fills the engine's log. That one warning is dropped; every other line
// of containerd's and BuildKit's log is written as it was.
func init() {
	l := logrus.StandardLogger()
	l.SetFormatter(quietFormatter{l.Formatter})
}

type quietFormatter struct{ logrus.Formatter }

func (f quietFormatter) Format(e *logrus.Entry) ([]byte, error) {
	if unsupportedFsverity(e) {
		return nil, nil
	}
	return f.Formatter.Format(e)
}

// unsupportedFsverity reports whether e is the content store's warning that
// fsverity is not supported, as on every system but Linux.
func unsupportedFsverity(e *logrus.Entry) bool {
	if e.Message != "failed check for fsverity support" {
		return false
	}
	err, _ := e.Data[logrus.ErrorKey].(error)
	return err != nil && strings.Contains(err.Error(), "only supported on Linux")
}
