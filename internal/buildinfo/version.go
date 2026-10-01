package buildinfo

import "runtime/debug"

func version(info *debug.BuildInfo) string {
	revision, dirty := "", false
	if info != nil {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				revision = setting.Value
			case "vcs.modified":
				dirty = setting.Value == "true"
			}
		}
	}
	if revision == "" {
		return "dev"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if dirty {
		revision += "-dirty"
	}
	return revision
}
func Version() string   { info, _ := debug.ReadBuildInfo(); return version(info) }
func UserAgent() string { return "claude-router/" + Version() }
