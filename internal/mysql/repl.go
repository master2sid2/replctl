package mysql

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/fatih/color"
)

// ReplicaStatus holds parsed output of SHOW REPLICA STATUS.
type ReplicaStatus struct {
	Host          string // the host we queried
	SourceHost    string
	SourcePort    string
	IORunning     string // "Yes" | "No"
	SQLRunning    string // "Yes" | "No"
	SecondsBehind string // numeric string or "" when NULL
	LastError     string
	IOState       string
	IsConfigured  bool
}

// IORunningBool returns true when IO thread is running.
func (s *ReplicaStatus) IORunningBool() bool { return s.IORunning == "Yes" }

// SQLRunningBool returns true when SQL thread is running.
func (s *ReplicaStatus) SQLRunningBool() bool { return s.SQLRunning == "Yes" }

// SecondsBehindInt parses SecondsBehind; returns -1 when NULL.
func (s *ReplicaStatus) SecondsBehindInt() int {
	v, err := strconv.Atoi(s.SecondsBehind)
	if err != nil {
		return -1
	}
	return v
}

// GetReplicaStatus queries SHOW REPLICA STATUS (with fallback to SHOW SLAVE STATUS).
func GetReplicaStatus(ctx context.Context, c *Client) (*ReplicaStatus, error) {
	m, err := c.QueryMap(ctx, "SHOW REPLICA STATUS")
	if err != nil {
		// Older MySQL / MariaDB syntax.
		m, err = c.QueryMap(ctx, "SHOW SLAVE STATUS")
		if err != nil {
			return nil, fmt.Errorf("replica status on %s: %w", c.Host(), err)
		}
	}

	s := &ReplicaStatus{Host: c.Host()}
	if m == nil {
		return s, nil // not configured
	}

	s.IsConfigured = true
	s.SourceHost = pick(m, "Source_Host", "Master_Host")
	s.SourcePort = pick(m, "Source_Port", "Master_Port")
	s.IORunning = pick(m, "Replica_IO_Running", "Slave_IO_Running")
	s.SQLRunning = pick(m, "Replica_SQL_Running", "Slave_SQL_Running")
	s.SecondsBehind = pick(m, "Seconds_Behind_Source", "Seconds_Behind_Master")
	s.LastError = pick(m, "Last_Error")
	s.IOState = pick(m, "Replica_IO_State", "Slave_IO_State")
	return s, nil
}

// CreateReplUser creates the replication user on src for dstIP.
func CreateReplUser(ctx context.Context, src *Client, dstIP, replUser, replPass string, dryRun bool) error {
	stmts := []string{
		"SET sql_log_bin=0",
		fmt.Sprintf(
			"CREATE USER IF NOT EXISTS '%s'@'%s' IDENTIFIED WITH mysql_native_password BY '%s'",
			replUser, dstIP, replPass,
		),
		fmt.Sprintf("GRANT REPLICATION SLAVE ON *.* TO '%s'@'%s'", replUser, dstIP),
		"SET sql_log_bin=1",
	}
	if dryRun {
		printDryRun("SRC ("+dstIP+" → src)", stmts)
		return nil
	}
	for _, s := range stmts {
		slog.Debug(s, "host", src.Host())
	}
	if err := src.ExecSession(ctx, stmts); err != nil {
		return err
	}
	color.New(color.FgYellow).Fprintf(os.Stderr, "  ✓ replication user '%s'@'%s' created on %s\n", replUser, dstIP, src.Host())
	return nil
}

// ConfigureReplication runs CHANGE REPLICATION SOURCE TO on dst with GTID auto-position.
func ConfigureReplication(ctx context.Context, dst *Client, srcIP string, srcPort int, replUser, replPass string, dryRun bool) error {
	stmts := []string{
		"STOP REPLICA",
		"RESET REPLICA ALL",
		fmt.Sprintf(
			"CHANGE REPLICATION SOURCE TO SOURCE_HOST='%s', SOURCE_PORT=%d, SOURCE_USER='%s', SOURCE_PASSWORD='%s', SOURCE_AUTO_POSITION=1",
			srcIP, srcPort, replUser, replPass,
		),
		"START REPLICA",
	}
	if dryRun {
		printDryRun(fmt.Sprintf("DST (← %s:%d)", srcIP, srcPort), stmts)
		return nil
	}
	// Each statement on its own call (STOP/START REPLICA don't need session state).
	for _, stmt := range stmts {
		slog.Debug(stmt, "host", dst.Host())
		if err := dst.ExecOne(ctx, stmt); err != nil {
			return fmt.Errorf("%q on %s: %w", stmt, dst.Host(), err)
		}
	}
	color.New(color.FgYellow).Fprintf(os.Stderr, "  ✓ replication configured on %s → %s:%d\n", dst.Host(), srcIP, srcPort)
	return nil
}

// pick returns the value of the first key found in m.
func pick(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return ""
}
