// Package platform holds what differs between operating systems, so the rest
// of the router is written once: an inter-process file lock, atomic file
// replacement, private directories and the per-user background service. Each
// has an implementation per OS selected at build time.
package platform
