package mysql

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/fatih/color"
	gmysql "github.com/go-sql-driver/mysql"
)

// EnsureClonePlugin installs the clone plugin on c if it is not already active.
func EnsureClonePlugin(ctx context.Context, c *Client) error {
	const q = "SELECT PLUGIN_STATUS FROM INFORMATION_SCHEMA.PLUGINS WHERE PLUGIN_NAME='clone'"
	slog.Debug(q, "host", c.Host())
	m, err := c.QueryMap(ctx, q)
	if err != nil {
		return fmt.Errorf("check clone plugin on %s: %w", c.Host(), err)
	}
	if m == nil || m["PLUGIN_STATUS"] != "ACTIVE" {
		const install = "INSTALL PLUGIN clone SONAME 'mysql_clone.so'"
		slog.Debug(install, "host", c.Host())
		if err := c.ExecOne(ctx, install); err != nil {
			return err
		}
	}
	color.New(color.FgYellow).Fprintf(os.Stderr, "  ✓ clone plugin active on %s\n", c.Host())
	return nil
}

// CreateCloneUsers creates clone_user on src (BACKUP_ADMIN) and dst (CLONE_ADMIN).
func CreateCloneUsers(ctx context.Context, src, dst *Client, srcIP, dstIP, clonePass string, dryRun bool) error {
	srcStmts := []string{
		"SET sql_log_bin=0",
		fmt.Sprintf("CREATE USER IF NOT EXISTS 'clone_user'@'%s' IDENTIFIED WITH mysql_native_password BY '%s'", dstIP, clonePass),
		fmt.Sprintf("GRANT BACKUP_ADMIN ON *.* TO 'clone_user'@'%s'", dstIP),
		"SET sql_log_bin=1",
	}
	dstStmts := []string{
		"SET sql_log_bin=0",
		fmt.Sprintf("CREATE USER IF NOT EXISTS 'clone_user'@'%s' IDENTIFIED WITH mysql_native_password BY '%s'", srcIP, clonePass),
		fmt.Sprintf("GRANT CLONE_ADMIN ON *.* TO 'clone_user'@'%s'", srcIP),
		"SET sql_log_bin=1",
	}

	if dryRun {
		printDryRun("SRC ("+srcIP+")", srcStmts)
		printDryRun("DST ("+dstIP+")", dstStmts)
		return nil
	}

	for _, s := range srcStmts {
		slog.Debug(s, "host", src.Host())
	}
	if err := src.ExecSession(ctx, srcStmts); err != nil {
		return fmt.Errorf("clone_user on src: %w", err)
	}
	color.New(color.FgYellow).Fprintf(os.Stderr, "  ✓ clone_user@%s created on %s (BACKUP_ADMIN)\n", dstIP, src.Host())

	for _, s := range dstStmts {
		slog.Debug(s, "host", dst.Host())
	}
	if err := dst.ExecSession(ctx, dstStmts); err != nil {
		return fmt.Errorf("clone_user on dst: %w", err)
	}
	color.New(color.FgYellow).Fprintf(os.Stderr, "  ✓ clone_user@%s created on %s (CLONE_ADMIN)\n", srcIP, dst.Host())
	return nil
}

// RunClone executes CLONE INSTANCE FROM on dst with per-stage progress display.
// Returns nil when the CLONE statement finishes (or connection drops due to restart).
// After this returns, call WaitForMySQL + VerifyCloneStatus to confirm success.
func RunClone(ctx context.Context, dst *Client, srcIP string, srcPort int, clonePass string, dryRun bool) error {
	donorSQL := fmt.Sprintf("SET GLOBAL clone_valid_donor_list = '%s:%d'", srcIP, srcPort)
	cloneSQL := fmt.Sprintf(
		"CLONE INSTANCE FROM 'clone_user'@'%s':%d IDENTIFIED BY '%s'",
		srcIP, srcPort, clonePass,
	)

	if dryRun {
		printDryRun(fmt.Sprintf("DST (%s:%d)", srcIP, srcPort), []string{donorSQL, cloneSQL})
		return nil
	}

	slog.Debug(donorSQL, "host", dst.Host())
	if err := dst.ExecOne(ctx, donorSQL); err != nil {
		return fmt.Errorf("set clone donor list: %w", err)
	}
	slog.Debug(cloneSQL, "host", dst.Host())

	cloneErrCh := make(chan error, 1)
	go func() {
		_, err := dst.db.ExecContext(ctx, cloneSQL)
		cloneErrCh <- err
	}()

	// Give MySQL a moment before polling progress.
	time.Sleep(2 * time.Second)

	tracker := newCloneTracker()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case err := <-cloneErrCh:
			// Final refresh so we don't miss the last completed stage.
			if rows, e := dst.QueryRows(context.Background(), cloneProgressQuery); e == nil {
				tracker.update(rows)
			}
			fmt.Fprintf(os.Stderr, "\r\033[K") // clear spinner line

			if err != nil {
				var mysqlErr *gmysql.MySQLError
				if errors.As(err, &mysqlErr) {
					return fmt.Errorf("clone MySQL error %d: %s", mysqlErr.Number, mysqlErr.Message)
				}
				// Connection drop = MySQL restarted after clone. Not an error here;
				// the caller must call WaitForMySQL + VerifyCloneStatus to confirm.
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					slog.Debug("clone connection dropped — MySQL is restarting")
					return nil
				}
				// Any other non-MySQL error is also treated as an expected restart.
				slog.Debug("clone connection reset", "detail", err.Error())
			}
			return nil

		case <-ticker.C:
			if rows, err := dst.QueryRows(context.Background(), cloneProgressQuery); err == nil {
				tracker.update(rows)
			}

		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "\r\033[K")
			return ctx.Err()
		}
	}
}

// VerifyCloneStatus reconnects to dst and checks performance_schema.clone_status.
// Returns nil if STATE = 'Completed', error if 'Failed', and logs a warning for anything else.
func VerifyCloneStatus(ctx context.Context, c *Client) error {
	m, err := c.QueryMap(ctx,
		"SELECT STATE, ERROR_NO, ERROR_MESSAGE FROM performance_schema.clone_status ORDER BY ID DESC LIMIT 1",
	)
	if err != nil || m == nil {
		// clone_status unavailable (older MySQL, or table empty on first clone).
		slog.Warn("cannot verify clone_status, assuming success", "err", err)
		return nil
	}
	switch m["STATE"] {
	case "Completed", "":
		color.New(color.FgYellow).Fprintf(os.Stderr, "  ✓ STATE = Completed\n")
		return nil
	case "Failed":
		msg := m["ERROR_MESSAGE"]
		if msg == "" {
			msg = "error code " + m["ERROR_NO"]
		}
		return fmt.Errorf("clone failed on server: %s", msg)
	default:
		slog.Warn("unexpected clone_status state", "state", m["STATE"])
		return nil
	}
}

// WaitForMySQL polls host:port until MySQL accepts connections, with exponential backoff.
func WaitForMySQL(host string, port int, user, pass string, maxWait time.Duration) error {
	deadline := time.Now().Add(maxWait)
	delay := 3 * time.Second

	fmt.Fprintf(os.Stderr, "  waiting for MySQL on %s to restart", host)
	attempt := 0
	for time.Now().Before(deadline) {
		time.Sleep(delay)
		if delay < 30*time.Second {
			delay = min(delay*2, 30*time.Second)
		}
		attempt++
		fmt.Fprintf(os.Stderr, ".")

		cfg := Config{Host: host, Port: port, User: user, Password: pass, Timeout: 5 * time.Second}
		c, err := NewClient(cfg)
		if err != nil {
			slog.Debug("reconnect attempt failed", "attempt", attempt, "err", err)
			continue
		}
		pingErr := c.Ping(context.Background())
		_ = c.Close()
		if pingErr == nil {
			color.New(color.FgYellow).Fprintf(os.Stderr, " ✓\n")
			return nil
		}
		slog.Debug("ping failed", "attempt", attempt, "err", pingErr)
	}
	return fmt.Errorf("MySQL on %s:%d did not come back within %s", host, port, maxWait)
}

// CleanupClone drops clone_user and uninstalls the clone plugin on both servers.
func CleanupClone(ctx context.Context, src, dst *Client, srcIP, dstIP string) {
	srcStmts := []string{
		"SET sql_log_bin=0",
		fmt.Sprintf("DROP USER IF EXISTS 'clone_user'@'%s'", dstIP),
		"UNINSTALL PLUGIN clone",
		"SET sql_log_bin=1",
	}
	dstStmts := []string{
		"SET sql_log_bin=0",
		fmt.Sprintf("DROP USER IF EXISTS 'clone_user'@'%s'", srcIP),
		"UNINSTALL PLUGIN clone",
		"SET sql_log_bin=1",
	}
	for _, s := range srcStmts {
		slog.Debug(s, "host", src.Host())
	}
	if err := src.ExecSession(ctx, srcStmts); err != nil {
		slog.Warn("cleanup on src", "err", err)
	}
	for _, s := range dstStmts {
		slog.Debug(s, "host", dst.Host())
	}
	if err := dst.ExecSession(ctx, dstStmts); err != nil {
		slog.Warn("cleanup on dst", "err", err)
	}
	color.New(color.FgYellow).Fprintf(os.Stderr, "  ✓ clone_user dropped, clone plugin uninstalled\n")
}

// ── per-stage progress tracker ───────────────────────────────────────────────

const cloneProgressQuery = `SELECT STAGE, STATE, BEGIN_TIME, END_TIME, ESTIMATE, DATA_ELAPSED
FROM performance_schema.clone_progress`

var spinChars = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type cloneTracker struct {
	printed   map[string]bool
	spinIdx   int
	stageSeen map[string]time.Time
}

func newCloneTracker() *cloneTracker {
	return &cloneTracker{
		printed:   make(map[string]bool),
		stageSeen: make(map[string]time.Time),
	}
}

// update processes a fresh snapshot of clone_progress rows.
func (t *cloneTracker) update(rows []map[string]string) {
	green := color.New(color.FgGreen)

	// Print newly completed stages first (preserving order from MySQL).
	for _, row := range rows {
		name := row["STAGE"]
		if row["STATE"] == "Completed" && !t.printed[name] {
			t.printed[name] = true
			dur := parseStageDuration(row["BEGIN_TIME"], row["END_TIME"])
			bytes, _ := strconv.ParseInt(row["DATA_ELAPSED"], 10, 64)

			fmt.Fprintf(os.Stderr, "\r\033[K") // erase spinner line
			if bytes > 0 {
				green.Fprintf(os.Stderr, "  ✓ %-12s  %-10s  %s\n",
					name, fmtDur(dur), fmtBytes(bytes))
			} else {
				green.Fprintf(os.Stderr, "  ✓ %-12s  %s\n",
					name, fmtDur(dur))
			}
		}
	}

	// Render spinner for current in-progress stage.
	spin := spinChars[t.spinIdx%len(spinChars)]
	t.spinIdx++

	for _, row := range rows {
		if row["STATE"] != "In Progress" {
			continue
		}
		name := row["STAGE"]
		if _, seen := t.stageSeen[name]; !seen {
			t.stageSeen[name] = time.Now()
		}
		elapsed := time.Since(t.stageSeen[name]).Round(time.Second)

		est, _ := strconv.ParseInt(row["ESTIMATE"], 10, 64)
		elp, _ := strconv.ParseInt(row["DATA_ELAPSED"], 10, 64)

		extra := ""
		if est > 0 && elp > 0 {
			extra = fmt.Sprintf("  %s / %s  %.0f%%",
				fmtBytes(elp), fmtBytes(est), float64(elp)/float64(est)*100)
		}

		fmt.Fprintf(os.Stderr, "\r  %s %-12s  %-8s%s    ",
			spin, name, fmtDur(elapsed), extra)
		return
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func parseStageDuration(begin, end string) time.Duration {
	const layout = "2006-01-02 15:04:05"
	bt, err1 := time.Parse(layout, begin)
	et, err2 := time.Parse(layout, end)
	if err1 != nil || err2 != nil || end == "" {
		return 0
	}
	return et.Sub(bt).Round(time.Second)
}

func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	if s == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dm%ds", m, s)
}

func fmtBytes(b int64) string {
	const (
		_KB = 1024
		_MB = 1024 * _KB
		_GB = 1024 * _MB
	)
	switch {
	case b >= _GB:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(_GB))
	case b >= _MB:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(_MB))
	case b >= _KB:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(_KB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func printDryRun(target string, stmts []string) {
	fmt.Printf("[dry-run] -- %s\n", target)
	for _, s := range stmts {
		fmt.Printf("  %s;\n", s)
	}
}
