package libsystem

import (
	"os/signal"
	"syscall"
)

func signalIgnore(s syscall.Signal) { signal.Ignore(s) }
