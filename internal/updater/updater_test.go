package updater

import "testing"

func TestIsNewer(t *testing.T) {
	tests := []struct {
		name    string
		latest  string
		current string
		want    bool
	}{
		{"patch bump", "v0.0.4", "v0.0.3", true},
		{"minor bump", "v0.1.0", "v0.0.9", true},
		{"major bump", "v1.0.0", "v0.9.9", true},
		{"equal", "v0.0.3", "v0.0.3", false},
		{"older patch", "v0.0.2", "v0.0.3", false},
		{"older minor", "v0.0.9", "v0.1.0", false},
		{"older major", "v0.9.9", "v1.0.0", false},
		{"no v prefix", "0.0.4", "0.0.3", true},
		{"mixed prefix", "v0.0.4", "0.0.3", true},
		{"invalid latest", "invalid", "v0.0.3", false},
		{"invalid current", "v0.0.3", "invalid", false},
		{"both invalid", "foo", "bar", false},
		{"empty latest", "", "v0.0.3", false},
		{"empty current", "v0.0.3", "", false},
		{"dev current", "v0.0.3", "dev", false},
		{"two part version", "v0.1", "v0.0.3", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isNewer(tt.latest, tt.current)
			if got != tt.want {
				t.Errorf("isNewer(%q, %q) = %v, want %v", tt.latest, tt.current, got, tt.want)
			}
		})
	}
}
