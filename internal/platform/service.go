package platform

import (
	"context"
	"time"
)

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

// Status is what the OS knows of a service: whether its definition is
// installed and whether it is loaded, that is, supervised and running.
type Status struct{ Installed, Loaded bool }

// Service installs and supervises per-user background services. The label
// names the service in every call after Install.
type Service interface {
	// Install writes the definition without loading it.
	Install(ctx context.Context, spec ServiceSpec) error
	// Uninstall unloads the service if it is loaded and removes the
	// definition.
	Uninstall(ctx context.Context, label string) error
	// Start loads the installed definition, which starts the program.
	Start(ctx context.Context, label string) error
	// Stop unloads the service, which stops the program.
	Stop(ctx context.Context, label string) error
	Status(ctx context.Context, label string) (Status, error)
}
