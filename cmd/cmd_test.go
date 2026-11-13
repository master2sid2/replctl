package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	mysqlpkg "github.com/master2sid2/replctl/internal/mysql"
)

func TestSplitHosts(t *testing.T) {
	got := splitHosts([]string{"r1,r2", " r3 ", "r4,,", ""})
	if want := []string{"r1", "r2", "r3", "r4"}; !slices.Equal(got, want) {
		t.Errorf("splitHosts = %v, want %v", got, want)
	}
}

func TestPairDst(t *testing.T) {
	saved := Cfg
	t.Cleanup(func() { Cfg = saved })

	tests := []struct {
		src     string
		dst     []string
		want    string
		wantErr string
	}{
		{"db1", []string{"db2"}, "db2", ""},
		{"", []string{"db2"}, "", "--src is required"},
		{"db1", nil, "", "--dst is required"},
		{"db1", []string{"db2", "db3"}, "", "exactly one destination"},
		{"db1", []string{"db2,db3"}, "", "exactly one destination"},
		{"db1", []string{"db1"}, "", "must be different"},
	}
	for _, tt := range tests {
		Cfg.Src, Cfg.Dst = tt.src, tt.dst
		got, err := pairDst()
		if got != tt.want || (tt.wantErr == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("pairDst(%q, %v) = %q, %v; want %q, %q", tt.src, tt.dst, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestConfigFile(t *testing.T) {
	home, xdg := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	write := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("port: 3307\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	current := filepath.Join(xdg, "replctl", "config.yaml")
	legacy := filepath.Join(home, ".mysqlctl.yaml")

	check := func(explicit, want string) {
		t.Helper()
		got, err := configFile(explicit)
		if err != nil || got != want {
			t.Errorf("configFile(%q) = %q, %v; want %q", explicit, got, err, want)
		}
	}
	check("", "") // nothing at all is fine
	write(legacy)
	check("", legacy)
	write(current)
	check("", current) // the new location wins
	check("/etc/replctl.yaml", "/etc/replctl.yaml")

	t.Setenv("XDG_CONFIG_HOME", "")
	write(filepath.Join(home, ".config", "replctl", "config.yaml"))
	check("", filepath.Join(home, ".config", "replctl", "config.yaml"))
}

func TestLagJSONUnknownLagIsNull(t *testing.T) {
	results := []lagResult{
		{host: "ok", status: &mysqlpkg.ReplicaStatus{Host: "ok", IsConfigured: true, IORunning: "Yes", SQLRunning: "Yes", SecondsBehind: "0"}},
		{host: "stopped", status: &mysqlpkg.ReplicaStatus{Host: "stopped", IsConfigured: true, IORunning: "No", SQLRunning: "Yes"}},
		{host: "down", err: os.ErrDeadlineExceeded},
	}
	var buf bytes.Buffer
	printLagJSON(&buf, results)
	out := buf.String()
	if strings.Count(out, `"seconds_behind": 0`) != 1 || strings.Count(out, `"seconds_behind": null`) != 2 {
		t.Errorf("unexpected seconds_behind values:\n%s", out)
	}
}
