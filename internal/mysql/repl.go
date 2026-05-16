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
func CreateReplUser(ctx context.Context, src *Client, dstIP, replUser, replPass string, dryRun, nativeAuth bool) error {
	stmts := []string{
		"SET sql_log_bin=0",
		fmt.Sprintf("DROP USER IF EXISTS '%s'@'%s'", replUser, dstIP),
		fmt.Sprintf(
			"CREATE USER '%s'@'%s' %s",
			replUser, dstIP, identifiedBy(replPass, nativeAuth),
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
	color.New(color.FgYellow).Fprintf(os.Stderr, "  ✓ %s@%s created on %s (REPLICATION SLAVE)\n", replUser, dstIP, src.Host())
	return nil
}

// ConfigureReplication runs CHANGE REPLICATION SOURCE TO on dst with GTID auto-position.
func ConfigureReplication(ctx context.Context, dst *Client, srcIP string, srcPort int, replUser, replPass string, dryRun bool) error {
	changeStmt := fmt.Sprintf(
		"CHANGE REPLICATION SOURCE TO SOURCE_HOST='%s', SOURCE_PORT=%d, SOURCE_USER='%s', SOURCE_PASSWORD='%s', SOURCE_AUTO_POSITION=1, SOURCE_SSL=1",
		srcIP, srcPort, replUser, replPass,
	)
	// Display label omits the password.
	changeLabel := fmt.Sprintf(
		"CHANGE REPLICATION SOURCE TO SOURCE_HOST='%s', SOURCE_AUTO_POSITION=1, SOURCE_SSL=1",
		srcIP,
	)
	type step struct{ sql, label string }
	steps := []step{
		{"STOP REPLICA", "STOP REPLICA"},
		{"RESET REPLICA ALL", "RESET REPLICA ALL"},
		{changeStmt, changeLabel},
		{"START REPLICA", "START REPLICA"},
	}
	if dryRun {
		stmts := make([]string, len(steps))
		for i, s := range steps {
			stmts[i] = s.sql
		}
		printDryRun(fmt.Sprintf("DST (← %s:%d)", srcIP, srcPort), stmts)
		return nil
	}
	yellow := color.New(color.FgYellow)
	for _, s := range steps {
		slog.Debug(s.sql, "host", dst.Host())
		if err := dst.ExecOne(ctx, s.sql); err != nil {
			return fmt.Errorf("%q on %s: %w", s.sql, dst.Host(), err)
		}
		yellow.Fprintf(os.Stderr, "  ✓ %s\n", s.label)
	}
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
