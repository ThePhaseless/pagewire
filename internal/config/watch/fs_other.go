//go:build !linux

package watch

// remoteFS is Linux-only; elsewhere the resync runs only when watching fails.
func remoteFS(string) bool { return false }
