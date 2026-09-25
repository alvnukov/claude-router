// Package platform holds what differs between operating systems, so the rest
// of the router is written once: an inter-process file lock, atomic file
// replacement and private directories. Each has a unix and a windows
// implementation selected at build time.
package platform
