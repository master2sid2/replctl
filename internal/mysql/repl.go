package mysql

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fatih/color"
)

// ReplicaStatus holds parsed output of SHOW REPLICA STATUS.
type ReplicaStatus struct {
	Host          string // the host we queried
	Channel       string // replication channel name; "" for the default channel
	SourceHost    string
	SourcePort    string
	IORunning     string // "Yes" | "No"
	SQLRunning    string // "Yes" | "No"
	SecondsBehind string // numeric string or "" when NULL
	LastError     string // IO and SQL thread errors combined
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

// GetReplicaStatuses queries SHOW REPLICA STATUS (with fallback to SHOW SLAVE STATUS)
// and returns one entry per replication channel; empty when replication is not configured.
func GetReplicaStatuses(ctx context.Context, c *Client) ([]*ReplicaStatus, error) {
	rows, err := c.QueryRows(ctx, "SHOW REPLICA STATUS")
	if err != nil {
		// Older MySQL / MariaDB syntax.
		rows, err = c.QueryRows(ctx, "SHOW SLAVE STATUS")
		if err != nil {
			return nil, fmt.Errorf("replica status on %s: %w", c.Host(), err)
		}
	}

	var out []*ReplicaStatus
	for _, m := range rows {
		s := &ReplicaStatus{Host: c.Host(), IsConfigured: true}
		s.Channel = pick(m, "Channel_Name")
		s.SourceHost = pick(m, "Source_Host", "Master_Host")
		s.SourcePort = pick(m, "Source_Port", "Master_Port")
		s.IORunning = pick(m, "Replica_IO_Running", "Slave_IO_Running")
		s.SQLRunning = pick(m, "Replica_SQL_Running", "Slave_SQL_Running")
		s.SecondsBehind = pick(m, "Seconds_Behind_Source", "Seconds_Behind_Master")
		s.IOState = pick(m, "Replica_IO_State", "Slave_IO_State")

		// Connection/auth/binlog problems are reported only in Last_IO_Error.
		var errs []string
		if e := pick(m, "Last_IO_Error"); e != "" {
			errs = append(errs, "IO: "+e)
		}
		if e := pick(m, "Last_SQL_Error", "Last_Error"); e != "" {
			errs = append(errs, "SQL: "+e)
		}
		s.LastError = strings.Join(errs, "; ")
		out = append(out, s)
	}
	return out, nil
}

// GetReplicaStatus returns the status of the first replication channel of c;
// IsConfigured is false when replication is not configured.
func GetReplicaStatus(ctx context.Context, c *Client) (*ReplicaStatus, error) {
	all, err := GetReplicaStatuses(ctx, c)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return &ReplicaStatus{Host: c.Host()}, nil
	}
	return all[0], nil
}

// CheckReplicationPrereqs verifies, before anything is changed on dst, that GTID
// replication from src to dst can work. A returned error means replication would
// certainly break; warnings describe risks the operator has to judge.
func CheckReplicationPrereqs(ctx context.Context, src, dst *Client) (warnings []string, err error) {
	si, err := GetServerInfo(ctx, src)
	if err != nil {
		return nil, err
	}
	di, err := GetServerInfo(ctx, dst)
	if err != nil {
		return nil, err
	}

	switch {
	case si.UUID == di.UUID:
		return nil, fmt.Errorf("source and destination are the same MySQL instance (server_uuid %s)", si.UUID)
	case si.ServerID == di.ServerID:
		return nil, fmt.Errorf("source and destination have the same server_id (%s)", si.ServerID)
	case si.ServerID == "0" || di.ServerID == "0":
		return nil, fmt.Errorf("server_id must be non-zero on both servers")
	case si.GTIDMode != "ON":
		return nil, fmt.Errorf("gtid_mode is %s on source %s, must be ON", si.GTIDMode, src.Host())
	case di.GTIDMode != "ON":
		return nil, fmt.Errorf("gtid_mode is %s on destination %s, must be ON", di.GTIDMode, dst.Host())
	case !si.LogBin:
		return nil, fmt.Errorf("binary logging is disabled on source %s", src.Host())
	}

	q := fmt.Sprintf(
		"SELECT GTID_SUBTRACT(@@global.gtid_purged, %[1]s) AS missing, GTID_SUBTRACT(%[1]s, @@global.gtid_executed) AS errant",
		quote(di.GTIDExecuted))
	m, err := src.QueryMap(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("compare GTID sets on %s: %w", src.Host(), err)
	}
	if missing := strings.Join(strings.Fields(m["missing"]), ""); missing != "" {
		return nil, fmt.Errorf(
			"source %s has already purged binary logs that destination still needs (%s): replication cannot start, re-clone the destination",
			src.Host(), truncate(missing, 200))
	}
	if errant := strings.Join(strings.Fields(m["errant"]), ""); errant != "" {
		warnings = append(warnings, fmt.Sprintf(
			"destination has transactions that source %s does not have: %s\n"+
				"    if the source never receives them, the data on destination will differ from the source",
			src.Host(), truncate(errant, 200)))
	}
	if !di.ReadOnly {
		warnings = append(warnings, fmt.Sprintf(
			"read_only is OFF on destination %s: writes made there will not exist on the source", dst.Host()))
	}
	return warnings, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// CreateReplUser creates the replication user on src for dstIP.
func CreateReplUser(ctx context.Context, src *Client, dstIP, replUser, replPass string, dryRun, nativeAuth bool) error {
	acct := account(replUser, dstIP)
	stmts := []string{
		"SET sql_log_bin=0",
		"DROP USER IF EXISTS " + acct,
		fmt.Sprintf("CREATE USER %s %s", acct, identifiedBy(replPass, nativeAuth)),
		"GRANT REPLICATION SLAVE ON *.* TO " + acct,
		"SET sql_log_bin=1",
	}
	if dryRun {
		printDryRun("SRC ("+dstIP+" → src)", stmts)
		return nil
	}
	for _, s := range stmts {
		slog.Debug(maskSQL(s), "host", src.Host())
	}
	if err := src.ExecSession(ctx, stmts); err != nil {
		return err
	}
	color.New(color.FgYellow).Fprintf(os.Stderr, "  ✓ %s@%s created on %s (REPLICATION SLAVE)\n", replUser, dstIP, src.Host())
	return nil
}

// ConfigureReplication runs CHANGE REPLICATION SOURCE TO on dst with GTID auto-position.
// Any replication already configured on dst is removed first (RESET REPLICA ALL), which
// is what repoints a replica from one source to another.
// With ssl=false the connection is not encrypted and the replica requests the source's
// RSA public key instead, as caching_sha2_password requires one of the two.
func ConfigureReplication(ctx context.Context, dst *Client, srcIP string, srcPort int, replUser, replPass string, ssl, dryRun bool) error {
	security := "SOURCE_SSL=1"
	if !ssl {
		security = "SOURCE_SSL=0, GET_SOURCE_PUBLIC_KEY=1"
	}
	changeStmt := fmt.Sprintf(
		"CHANGE REPLICATION SOURCE TO SOURCE_HOST=%s, SOURCE_PORT=%d, SOURCE_USER=%s, SOURCE_PASSWORD=%s, SOURCE_AUTO_POSITION=1, %s",
		quote(srcIP), srcPort, quote(replUser), quote(replPass), security,
	)
	steps := []string{"STOP REPLICA", "RESET REPLICA ALL", changeStmt, "START REPLICA"}
	if dryRun {
		printDryRun(fmt.Sprintf("DST (← %s:%d)", srcIP, srcPort), steps)
		return nil
	}
	yellow := color.New(color.FgYellow)
	for _, s := range steps {
		masked := maskSQL(s)
		slog.Debug(masked, "host", dst.Host())
		if err := dst.ExecOne(ctx, s); err != nil {
			return fmt.Errorf("%q on %s: %w", masked, dst.Host(), err)
		}
		yellow.Fprintf(os.Stderr, "  ✓ %s\n", masked)
	}
	return nil
}

// WaitReplicaRunning polls dst until both replication threads run, an error is
// reported by either of them, or maxWait elapses. The last status is returned
// together with the error so the caller can still display it.
func WaitReplicaRunning(ctx context.Context, dst *Client, maxWait time.Duration) (*ReplicaStatus, error) {
	deadline := time.Now().Add(maxWait)
	healthy := 0
	for {
		s, err := GetReplicaStatus(ctx, dst)
		if err != nil {
			return nil, err
		}
		switch {
		case s.IORunningBool() && s.SQLRunningBool():
			// The IO thread can report Yes for a moment before the source rejects
			// it, so require the state to hold across consecutive polls.
			if healthy++; healthy >= 3 {
				return s, nil
			}
		case s.LastError != "":
			return s, fmt.Errorf("replication is not running on %s: %s", dst.Host(), s.LastError)
		case time.Now().After(deadline):
			return s, fmt.Errorf("replication did not start on %s within %s (IO=%s, SQL=%s)",
				dst.Host(), maxWait, s.IORunning, s.SQLRunning)
		}
		select {
		case <-ctx.Done():
			return s, ctx.Err()
		case <-time.After(time.Second):
		}
	}
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
