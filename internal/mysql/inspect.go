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
