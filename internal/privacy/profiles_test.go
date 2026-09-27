package privacy

import "testing"

func TestProfilesResolveExactlyOnePolicy(t *testing.T) {
	c, err := ParseProfiles([]byte(`{"version":1,"enabled":true,"default":"base","profiles":[{"id":"base","name":"Base","enabled":true,"rules":{}},{"id":"off","name":"Off","enabled":false,"rules":{}}],"bindings":[{"kind":"provider","target":"cloud","profile":"off"},{"kind":"pool","target":"work/fast","profile":"base"},{"kind":"model","target":"cloud/m","profile":"off"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		target  Target
		id, via string
		active  bool
	}{
		{Target{}, "base", "default", true},
		{Target{Provider: "cloud"}, "off", "provider", false},
		{Target{Provider: "cloud", Pool: "work/fast"}, "base", "pool", true},
		{Target{Provider: "cloud", Pool: "work/fast", Model: "cloud/m"}, "off", "model", false},
	} {
		r := c.Resolve(tc.target)
		if r.Profile != tc.id || r.Via != tc.via || r.Enabled != tc.active {
			t.Fatalf("resolution = %+v", r)
		}
	}
	for _, bad := range []string{
		`{"version":1,"profiles":[],"default":"missing"}`,
		`{"version":1,"profiles":[{"id":"x","name":"X","rules":{"filters":{"typo":false}}}]}`,
		`{"version":1,"profiles":[{"id":"x","name":"X","rules":{}}],"bindings":[{"kind":"model","target":"p/m","profile":"x"},{"kind":"model","target":"p/m","profile":"x"}]}`,
	} {
		if _, err := ParseProfiles([]byte(bad)); err == nil {
			t.Fatal("invalid profile set accepted")
		}
	}
}
