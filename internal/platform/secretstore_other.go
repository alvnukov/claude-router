//go:build !darwin && !windows

package platform

// OSSecretStore reports that this OS has no store the router uses; masking
// then fails closed instead of keeping the key in a file.
func OSSecretStore(service string) (SecretStore, error) {
	if err := checkSecretName("service", service); err != nil {
		return nil, err
	}
	return nil, ErrNoSecretStore
}
