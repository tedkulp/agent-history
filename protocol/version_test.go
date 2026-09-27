package protocol

import "testing"

func TestVersionLess(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"0.3.9", "0.4.0", true},
		{"0.4.0", "0.4.0", false},
		{"0.4.1", "0.4.0", false},
		{"0.4.0-rc.1", "0.4.0", false},
		{"0.4.0", "0.4.0-rc.1", false},
		{"0.4.0+build.7", "0.4.0", false},
		{"0.3.9-rc.1+x", "0.4.0", true},
		{"0.10.0", "0.9.0", false},
		{"1.0.0", "0.99.99", false},
		{"0.0.0-dev", "0.1.0", true},
	} {
		if got := VersionLess(tc.a, tc.b); got != tc.want {
			t.Errorf("VersionLess(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestValidVersion(t *testing.T) {
	for v, want := range map[string]bool{
		"0.4.0":          true,
		"0.4.0-rc.1":     true,
		"1.2.3+build.5":  true,
		"1.2.3-rc.1+b.2": true,
		"0.0.0-dev":      true,
		"v0.4.0":         false,
		"0.4":            false,
		"0.4.0.1":        false,
		"01.4.0":         false,
		"0.4.x":          false,
		"0.4.0-":         false,
		"0.4.0-rc..1":    false,
		"":               false,
	} {
		if got := ValidVersion(v); got != want {
			t.Errorf("ValidVersion(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestEffectiveMinCollectorVersion(t *testing.T) {
	for _, tc := range []struct {
		env, want string
		err       bool
	}{
		{"", MinCollectorVersion, false},
		{"99.0.0", "99.0.0", false},
		{"0.0.1", MinCollectorVersion, false},
		{"0.0.0-dev", MinCollectorVersion, false},
		{"not-a-version", "", true},
	} {
		got, err := EffectiveMinCollectorVersion(tc.env)
		if (err != nil) != tc.err || got != tc.want {
			t.Errorf("EffectiveMinCollectorVersion(%q) = %q, %v; want %q, err %v", tc.env, got, err, tc.want, tc.err)
		}
	}
}

func TestCollectorVersionFromUserAgent(t *testing.T) {
	for ua, want := range map[string]string{
		"agent-history-collector/0.3.1":           "0.3.1",
		"agent-history-collector/0.0.0-dev":       "0.0.0-dev",
		"agent-history-collector/0.3.1 extra/1.0": "0.3.1",
		"Go-http-client/1.1":                      "",
		"agent-history-collector":                 "",
		"":                                        "",
		"agent-history-collector/":                "",
		"agent-history-collectorx/0.3.1":          "",
	} {
		if got := CollectorVersion(ua); got != want {
			t.Errorf("CollectorVersion(%q) = %q, want %q", ua, got, want)
		}
	}
}
