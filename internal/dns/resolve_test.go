package dns

import "testing"

func TestResolve(t *testing.T) {
	tests := []struct{ in, want string }{
		{"10.0.0.1", "10.0.0.1"},
		{"::1", "::1"},
		{"::ffff:10.0.0.2", "10.0.0.2"},
		{"localhost", "127.0.0.1"},
	}
	for _, tt := range tests {
		got, err := Resolve(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
	if _, err := Resolve("no-such-host.invalid"); err == nil {
		t.Error("expected an error for an unresolvable host")
	}
}
