package platform

import "time"

// ServiceSpec describes a per-user background service: the program, where it
// runs and how the OS supervises it.
type ServiceSpec struct {
	Label string   // unique name, e.g. com.claude-local-router
	Exe   string   // absolute path of the program
	Args  []string // arguments after Exe
	// Env is set for the process; an empty map writes nothing.
	Env     map[string]string
	Dir     string // working directory
	LogPath string // stdout and stderr, appended
	// KeepAlive restarts the program whenever it exits.
	KeepAlive bool
	// ThrottleInterval is the least time between restarts and ExitTimeout
	// the wait between the stop signal and a kill, both in whole seconds.
	// Zero leaves the OS default.
	ThrottleInterval, ExitTimeout time.Duration
}
