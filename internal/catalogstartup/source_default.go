//go:build !catalogsynthetic

package catalogstartup

// ForProcess leaves catalog endpoints unchanged in ordinary builds, even when
// an environment variable for the synthetic test harness is present.
func ForProcess(officialURL, codexBaseURL string) (Dependencies, error) {
	return Production(officialURL, codexBaseURL), nil
}

func (d Dependencies) ValidateProvider(name, kind, baseURL string, authID ...string) error {
	return nil
}
