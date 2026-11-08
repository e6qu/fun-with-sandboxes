//go:build !darwin

package image

import "os"

// recordMode records nothing: containers, whose library presents the
// record, run on macOS only.
func recordMode(path string, perm, onDisk os.FileMode) error { return nil }

// RecordedMode is the file's own mode.
func RecordedMode(path string, fi os.FileInfo) os.FileMode { return fi.Mode().Perm() }
