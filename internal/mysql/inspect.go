package mysql

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// ServerInfo holds the server settings used by pre-flight checks.
type ServerInfo struct {
	UUID         string
	ServerID     string
	Version      string
	GTIDMode     string
	GTIDExecuted string
	LogBin       bool
	ReadOnly     bool
}

// GetServerInfo reads identity and replication-related settings of c.
func GetServerInfo(ctx context.Context, c *Client) (*ServerInfo, error) {
	const q = `SELECT @@global.server_uuid AS uuid, @@global.server_id AS server_id,
@@global.version AS version, @@global.gtid_mode AS gtid_mode,
@@global.gtid_executed AS gtid_executed, @@global.log_bin AS log_bin,
@@global.read_only AS read_only`
	m, err := c.QueryMap(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("read server settings on %s: %w", c.Host(), err)
	}
	return &ServerInfo{
		UUID:     m["uuid"],
		ServerID: m["server_id"],
		Version:  m["version"],
		GTIDMode: m["gtid_mode"],
		// Long GTID sets are returned with embedded newlines.
		GTIDExecuted: strings.Join(strings.Fields(m["gtid_executed"]), ""),
		LogBin:       m["log_bin"] == "1",
		ReadOnly:     m["read_only"] == "1",
	}, nil
}

// DataSummary describes what lives on a server, to show before it is overwritten.
type DataSummary struct {
	Schemas       int   // user schemas
	Bytes         int64 // data + index size of user schemas
	Replicas      int   // replicas currently connected to this server
	EnabledEvents int   // Event Scheduler events that are ENABLED
}

// GetDataSummary collects DataSummary for c.
func GetDataSummary(ctx context.Context, c *Client) (*DataSummary, error) {
	const sys = `('mysql','sys','information_schema','performance_schema')`
	const q = `SELECT
(SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name NOT IN ` + sys + `) AS n_schemas,
(SELECT COALESCE(SUM(data_length + index_length), 0) FROM information_schema.tables WHERE table_schema NOT IN ` + sys + `) AS n_bytes,
(SELECT COUNT(*) FROM information_schema.processlist WHERE command LIKE 'Binlog Dump%') AS n_replicas,
(SELECT COUNT(*) FROM information_schema.events WHERE status = 'ENABLED') AS n_events`
	m, err := c.QueryMap(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", c.Host(), err)
	}
	d := &DataSummary{}
	d.Schemas, _ = strconv.Atoi(m["n_schemas"])
	d.Bytes, _ = strconv.ParseInt(m["n_bytes"], 10, 64)
	d.Replicas, _ = strconv.Atoi(m["n_replicas"])
	d.EnabledEvents, _ = strconv.Atoi(m["n_events"])
	return d, nil
}

// CheckCloneVersions reports why a server running donor cannot be cloned onto
// one running recipient, before the clone is started: the clone plugin needs
// 8.0.17+ and the same version series on both sides; before 8.0.37 the exact
// same version. Versions that cannot be parsed are left to the server to judge.
func CheckCloneVersions(donor, recipient string) error {
	d, ok1 := parseVersion(donor)
	r, ok2 := parseVersion(recipient)
	if !ok1 || !ok2 {
		return nil
	}
	sameSeries := d[0] == r[0] && d[1] == r[1]
	switch {
	case !atLeast(d, 8, 0, 17) || !atLeast(r, 8, 0, 17):
		return fmt.Errorf("the clone plugin needs MySQL 8.0.17 or later (source %s, destination %s)", donor, recipient)
	case !sameSeries:
		return fmt.Errorf("source runs MySQL %s and destination %s: clone needs the same version series on both", donor, recipient)
	case d[2] != r[2] && d[0] == 8 && d[1] == 0 && min(d[2], r[2]) < 37:
		return fmt.Errorf("source runs MySQL %s and destination %s: before 8.0.37 clone needs exactly the same version", donor, recipient)
	}
	return nil
}

// parseVersion reads "8.4.3" out of version strings like "8.4.3-log".
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v, _, _ = strings.Cut(v, "-")
	parts := strings.Split(v, ".")
	if len(parts) < 3 {
		return out, false
	}
	for i := range out {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func atLeast(v [3]int, major, minor, patch int) bool {
	return v[0] > major || v[0] == major && (v[1] > minor || v[1] == minor && v[2] >= patch)
}
