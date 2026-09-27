package privacy

// FilterIDs names independently configurable detector families. Restoration
// always uses the same immutable engine snapshot as masking.
func FilterIDs() []string {
	return []string{"ipv4", "ipv6", "mac", "host", "email", "phone", "secret", "dictionary", "patterns", "fields", "sources"}
}

func enabled(filters map[string]bool, id string) bool {
	v, exists := filters[id]
	return !exists || v
}
