package mysql

import (
	"strings"
	"testing"
	"time"
)

func TestDriverConfig(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		addr     string
		timeout  time.Duration
		readTO   time.Duration
		password string
	}{
		{"defaults", Config{Host: "db1", Port: 3306}, "db1:3306", 30 * time.Second, 30 * time.Second, ""},
		{"IPv6 host", Config{Host: "::1", Port: 3307}, "[::1]:3307", 30 * time.Second, 30 * time.Second, ""},
		{"no read timeout", Config{Host: "db1", Port: 3306, Timeout: 5 * time.Second, ReadTimeout: -1}, "db1:3306", 5 * time.Second, 0, ""},
		{"own read timeout", Config{Host: "db1", Port: 3306, ReadTimeout: time.Minute}, "db1:3306", 30 * time.Second, time.Minute, ""},
		{"password with DSN syntax", Config{Host: "db1", Port: 3306, Password: "p@ss:w/rd?x=1)"}, "db1:3306", 30 * time.Second, 30 * time.Second, "p@ss:w/rd?x=1)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.cfg.driverConfig()
			if c.Addr != tt.addr || c.Timeout != tt.timeout || c.ReadTimeout != tt.readTO || c.Passwd != tt.password {
				t.Errorf("got addr=%s timeout=%s read=%s pass=%q", c.Addr, c.Timeout, c.ReadTimeout, c.Passwd)
			}
			if !c.InterpolateParams || !c.ParseTime {
				t.Error("InterpolateParams and ParseTime must be on")
			}
		})
	}
}

func TestCheckCloneVersions(t *testing.T) {
	tests := []struct {
		donor, recipient string
		wantErr          string // substring; "" = allowed
	}{
		{"8.4.3", "8.4.3", ""},
		{"8.4.3", "8.4.6-log", ""},
		{"8.0.37", "8.0.40", ""},
		{"8.0.36", "8.0.40", "exactly the same version"},
		{"8.0.30", "8.0.30", ""},
		{"8.0.46", "8.4.13", "same version series"},
		{"8.0.16", "8.0.16", "8.0.17 or later"},
		{"9.1.0", "9.1.0", ""},
		{"weird", "8.4.3", ""}, // unknown format: the server decides
	}
	for _, tt := range tests {
		t.Run(tt.donor+"→"+tt.recipient, func(t *testing.T) {
			err := CheckCloneVersions(tt.donor, tt.recipient)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestCheckReplicationPair(t *testing.T) {
	good := func() (*ServerInfo, *ServerInfo) {
		return &ServerInfo{UUID: "a", ServerID: "1", GTIDMode: "ON", LogBin: true},
			&ServerInfo{UUID: "b", ServerID: "2", GTIDMode: "ON", LogBin: true, ReadOnly: true}
	}
	tests := []struct {
		name    string
		mutate  func(si, di *ServerInfo)
		wantErr string
	}{
		{"fine", func(si, di *ServerInfo) {}, ""},
		{"same instance", func(si, di *ServerInfo) { di.UUID = "a" }, "same MySQL instance"},
		{"same server_id", func(si, di *ServerInfo) { di.ServerID = "1" }, "same server_id"},
		{"zero server_id", func(si, di *ServerInfo) { si.ServerID, di.ServerID = "0", "0" }, "non-zero"},
		{"GTID off on source", func(si, di *ServerInfo) { si.GTIDMode = "OFF" }, "on source"},
		{"GTID permissive on destination", func(si, di *ServerInfo) { di.GTIDMode = "ON_PERMISSIVE" }, "on destination"},
		{"no binlog on source", func(si, di *ServerInfo) { si.LogBin = false }, "binary logging"},
		{"no binlog on destination is fine", func(si, di *ServerInfo) { di.LogBin = false }, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			si, di := good()
			tt.mutate(si, di)
			err := checkReplicationPair(si, di, "src", "dst")
			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}

	_, di := good()
	if w := replicationWarnings(di, "dst"); len(w) != 0 {
		t.Errorf("read-only destination: unexpected warnings %v", w)
	}
	di.ReadOnly = false
	if w := replicationWarnings(di, "dst"); len(w) != 1 || !strings.Contains(w[0], "read_only is OFF") {
		t.Errorf("writable destination: warnings = %v", w)
	}
}

func TestFormatting(t *testing.T) {
	durs := map[time.Duration]string{
		0: "< 1s", 400 * time.Millisecond: "< 1s", 59 * time.Second: "59s", 2 * time.Minute: "2m",
		3*time.Minute + 5*time.Second: "3m5s", 2 * time.Hour: "2h", 26*time.Hour + 30*time.Minute: "1d2h",
	}
	for d, want := range durs {
		if got := fmtDur(d); got != want {
			t.Errorf("fmtDur(%s) = %q, want %q", d, got, want)
		}
	}
	sizes := map[int64]string{512: "512 B", 2048: "2.0 KB", 295 << 20: "295.0 MB", 64 << 30: "64.0 GB", 3 << 40: "3.0 TB"}
	for b, want := range sizes {
		if got := fmtBytes(b); got != want {
			t.Errorf("fmtBytes(%d) = %q, want %q", b, got, want)
		}
	}
	if got := parseStageDuration("2026-11-13 10:00:00.000000", "2026-11-13 10:01:30.500000"); got != 91*time.Second {
		t.Errorf("parseStageDuration = %s", got)
	}
	if got := parseStageDuration("2026-11-13 10:00:00", ""); got != 0 {
		t.Errorf("unfinished stage = %s", got)
	}
}
