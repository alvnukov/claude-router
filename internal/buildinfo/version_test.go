package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestVersionUsesActualBuildRevision(t *testing.T) {
	for _, tc := range []struct {
		settings []debug.BuildSetting
		want     string
	}{{nil, "dev"}, {[]debug.BuildSetting{{Key: "vcs.revision", Value: "123456789abcdef"}}, "123456789abc"}, {[]debug.BuildSetting{{Key: "vcs.revision", Value: "123456789abcdef"}, {Key: "vcs.modified", Value: "true"}}, "123456789abc-dirty"}} {
		if got := version(&debug.BuildInfo{Settings: tc.settings}); got != tc.want {
			t.Errorf("got %s want %s", got, tc.want)
		}
	}
}
