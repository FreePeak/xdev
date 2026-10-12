//go:build windows

package serve

// processAlive has no portable Windows spelling; the registration's pid check
// falls back to the /info probe, which is what actually decides whether a
// registered daemon is the one answering.
func processAlive(pid int) bool { return pid > 0 }
