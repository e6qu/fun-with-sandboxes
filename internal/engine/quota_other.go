//go:build !darwin

package engine

import "errors"

var errNoDiskImages = errors.New("a root size quota needs macOS's disk images")

func makeQuotaRoot(dir, staged, root string, quota int64) error { return errNoDiskImages }

func attachQuotaRoot(dir, root string) error { return errNoDiskImages }

func detachQuotaRoot(root string) error { return errNoDiskImages }

func mounted(dir, root string) bool { return false }
