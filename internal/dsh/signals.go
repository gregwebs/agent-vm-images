package dsh

import (
	"os"
	"os/signal"
	"syscall"
)

// signalNotify and stopSignals keep the signal vocabulary in one place: a hook
// installs the handlers once, and a test can drive the same code path without
// touching the process's own signal disposition.
func signalNotify(signals chan<- os.Signal) {
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
}

func stopSignals(signals chan<- os.Signal) {
	signal.Stop(signals)
	close(signals)
}
