package platform

import (
	"os"
	"path/filepath"
)

// NewService returns the user's launchd agents in ~/Library/LaunchAgents.
func NewService() (Service, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return Launchd{Dir: filepath.Join(home, "Library", "LaunchAgents"), Domain: LaunchdDomain(), Run: RunLaunchctl}, nil
}
