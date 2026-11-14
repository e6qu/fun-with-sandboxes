//go:build !darwin

package engine

import "errors"

var errNoGuard = errors.New("the machine guards measure macOS alone")

func pressureLevel() (int, error) { return 0, errNoGuard }

func diskFree(path string) (uint64, error) { return 0, errNoGuard }
