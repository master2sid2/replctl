package mysql

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
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

// identifiedBy returns the IDENTIFIED clause for CREATE USER.
// With nativeAuth=true uses mysql_native_password for compatibility with MySQL ≤ 8.0.
func identifiedBy(password string, nativeAuth bool) string {
	if nativeAuth {
		return fmt.Sprintf("IDENTIFIED WITH mysql_native_password BY '%s'", password)
	}
	return fmt.Sprintf("IDENTIFIED BY '%s'", password)
}

// CreateCloneUsers creates clone_user on src (BACKUP_ADMIN) and dst (CLONE_ADMIN).
func CreateCloneUsers(ctx context.Context, src, dst *Client, srcIP, dstIP, clonePass string, dryRun, nativeAuth bool) error {
	srcStmts := []string{
		"SET sql_log_bin=0",
		fmt.Sprintf("CREATE USER IF NOT EXISTS 'clone_user'@'%s' %s", dstIP, identifiedBy(clonePass, nativeAuth)),
		fmt.Sprintf("GRANT BACKUP_ADMIN ON *.* TO 'clone_user'@'%s'", dstIP),
		"SET sql_log_bin=1",
	}
	dstStmts := []string{
		"SET sql_log_bin=0",
		fmt.Sprintf("CREATE USER IF NOT EXISTS 'clone_user'@'%s' %s", srcIP, identifiedBy(clonePass, nativeAuth)),
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
		printDryRun(fmt.Sprintf("DST (← %s:%d)", srcIP, srcPort), []string{donorSQL, cloneSQL})
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
	dataTicker := time.NewTicker(2 * time.Second)
	spinTicker := time.NewTicker(100 * time.Millisecond)
	defer dataTicker.Stop()
	defer spinTicker.Stop()

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

		case <-dataTicker.C:
			if rows, err := dst.QueryRows(context.Background(), cloneProgressQuery); err == nil {
				tracker.update(rows)
			}

		case <-spinTicker.C:
			tracker.spin()

		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "\r\033[K")
			return ctx.Err()
		}
	}
}

// VerifyCloneStatus reconnects to dst and checks performance_schema.clone_status.
// If STATE is 'In Progress' (connection dropped but clone is still running), it polls
// until the clone completes or fails. Returns nil only when STATE = 'Completed'.
func VerifyCloneStatus(ctx context.Context, c *Client) error {
	const q = "SELECT STATE, ERROR_NO, ERROR_MESSAGE FROM performance_schema.clone_status ORDER BY ID DESC LIMIT 1"
	yellow := color.New(color.FgYellow)
	first := true
	for {
		m, err := c.QueryMap(ctx, q)
		if err != nil || m == nil {
			// clone_status unavailable (older MySQL, or table empty on first clone).
			slog.Warn("cannot verify clone_status, assuming success", "err", err)
			return nil
		}
		switch m["STATE"] {
		case "Completed", "":
			yellow.Fprintf(os.Stderr, "  ✓ STATE = Completed\n")
			return nil
		case "Failed":
			msg := m["ERROR_MESSAGE"]
			if msg == "" {
				msg = "error code " + m["ERROR_NO"]
			}
			return fmt.Errorf("clone failed on server: %s", msg)
		case "Incomplete":
			return fmt.Errorf("clone incomplete on server: operation was interrupted")
		case "In Progress":
			if first {
				fmt.Fprintf(os.Stderr, "  clone still running on server, waiting...\n")
				first = false
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
			continue
		default:
			slog.Warn("unexpected clone_status state", "state", m["STATE"])
			return nil
		}
	}
}

// WaitForMySQL polls host:port until MySQL accepts connections, with exponential backoff.
func WaitForMySQL(host string, port int, user, pass string, maxWait time.Duration) error {
	deadline := time.Now().Add(maxWait)
	delay := 3 * time.Second

	yellow := color.New(color.FgYellow)
	attempt := 0
	for time.Now().Before(deadline) {
		time.Sleep(delay)
		if delay < 30*time.Second {
			delay = min(delay*2, 30*time.Second)
		}
		attempt++

		switch attempt {
		case 1:
			fmt.Fprintf(os.Stderr, "  MySQL on %s is shutting down...\n", host)
		case 2:
			fmt.Fprintf(os.Stderr, "  Waiting for restart...\n")
		default:
			fmt.Fprintf(os.Stderr, "  Waiting for restart... (%d)\n", attempt-1)
		}

		cfg := Config{Host: host, Port: port, User: user, Password: pass, Timeout: 5 * time.Second}
		c, err := NewClient(cfg)
		if err != nil {
			slog.Debug("reconnect attempt failed", "attempt", attempt, "err", err)
			continue
		}
		pingErr := c.Ping(context.Background())
		_ = c.Close()
		if pingErr == nil {
			yellow.Fprintf(os.Stderr, "  MySQL is back online ✓\n")
			return nil
		}
		slog.Debug("ping failed", "attempt", attempt, "err", pingErr)
	}
	return fmt.Errorf("MySQL on %s:%d did not come back within %s", host, port, maxWait)
}

// CleanupClone drops clone_user and uninstalls the clone plugin on both servers.
// Uses its own context so cleanup runs even when the caller's context is cancelled.
func CleanupClone(_ context.Context, src, dst *Client, srcIP, dstIP string) {
	cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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
	if err := src.ExecSession(cleanCtx, srcStmts); err != nil {
		slog.Debug("cleanup on src", "err", err)
	}
	for _, s := range dstStmts {
		slog.Debug(s, "host", dst.Host())
	}
	if err := dst.ExecSession(cleanCtx, dstStmts); err != nil {
		slog.Debug("cleanup on dst", "err", err)
	}
	color.New(color.FgYellow).Fprintf(os.Stderr, "  ✓ clone_user dropped on both servers\n")
	color.New(color.FgYellow).Fprintf(os.Stderr, "  ✓ clone plugin uninstalled\n")
}

// ── per-stage progress tracker ───────────────────────────────────────────────

const cloneProgressQuery = `SELECT STAGE, STATE, BEGIN_TIME, END_TIME, ESTIMATE, DATA
FROM performance_schema.clone_progress`

var spinChars = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type cloneTracker struct {
	printed   map[string]bool
	spinIdx   int
	stageSeen map[string]time.Time
	lastRows  []map[string]string
}

func newCloneTracker() *cloneTracker {
	return &cloneTracker{
		printed:   make(map[string]bool),
		stageSeen: make(map[string]time.Time),
	}
}

// update processes a fresh snapshot: prints newly completed stages, then renders spinner.
func (t *cloneTracker) update(rows []map[string]string) {
	t.lastRows = rows
	green := color.New(color.FgGreen)

	for _, row := range rows {
		name := row["STAGE"]
		if row["STATE"] == "Completed" && !t.printed[name] {
			t.printed[name] = true
			dur := parseStageDuration(row["BEGIN_TIME"], row["END_TIME"])
			bytes, _ := strconv.ParseInt(row["DATA"], 10, 64)

			fmt.Fprintf(os.Stderr, "\r\033[K")
			if bytes > 0 {
				green.Fprintf(os.Stderr, "  ✓ %-12s  %-10s  %s\n",
					name, fmtDur(dur), fmtBytes(bytes))
			} else {
				green.Fprintf(os.Stderr, "  ✓ %-12s  %s\n",
					name, fmtDur(dur))
			}
		}
	}

	t.renderSpinner()
}

// spin advances the spinner frame using cached data (no DB query).
func (t *cloneTracker) spin() {
	t.renderSpinner()
}

func (t *cloneTracker) renderSpinner() {
	spin := spinChars[t.spinIdx%len(spinChars)]
	t.spinIdx++

	for _, row := range t.lastRows {
		if row["STATE"] != "In Progress" {
			continue
		}
		name := row["STAGE"]
		if _, seen := t.stageSeen[name]; !seen {
			t.stageSeen[name] = time.Now()
		}
		elapsed := time.Since(t.stageSeen[name]).Round(time.Second)

		est, _ := strconv.ParseInt(row["ESTIMATE"], 10, 64)
		elp, _ := strconv.ParseInt(row["DATA"], 10, 64)

		extra := ""
		if est > 0 && elp > 0 {
			pct := float64(elp) / float64(est)
			eta := ""
			if pct > 0.001 && pct < 0.999 {
				etaDur := time.Duration(float64(elapsed) * (1 - pct) / pct).Round(time.Second)
				eta = fmt.Sprintf("   ETA ~%s", fmtDur(etaDur))
			}
			extra = fmt.Sprintf("   %7s / %-7s  %5.1f%%%s",
				fmtBytes(elp), fmtBytes(est), pct*100, eta)
		}

		fmt.Fprintf(os.Stderr, "\r  %s %-12s  %-8s%s    ",
			spin, name, fmtDur(elapsed), extra)
		return
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

var stageTimeLayouts = []string{
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05.999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05.999999Z07:00",
	"2006-01-02T15:04:05Z07:00",
}

func parseStageTime(s string) (time.Time, bool) {
	for _, layout := range stageTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	slog.Debug("clone_progress: unrecognized timestamp format", "value", s)
	return time.Time{}, false
}

func parseStageDuration(begin, end string) time.Duration {
	if end == "" {
		return 0
	}
	bt, ok1 := parseStageTime(begin)
	et, ok2 := parseStageTime(end)
	if !ok1 || !ok2 {
		return 0
	}
	return et.Sub(bt).Round(time.Second)
}

func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	if d == 0 {
		return "< 1s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm%ds", m, s)
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	if h == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, h)
}

func fmtBytes(b int64) string {
	const (
		_KB = 1024
		_MB = 1024 * _KB
		_GB = 1024 * _MB
		_TB = 1024 * _GB
	)
	switch {
	case b >= _TB:
		return fmt.Sprintf("%.1f TB", float64(b)/float64(_TB))
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

var rePassword = regexp.MustCompile(`(?i)(BY\s+|PASSWORD=)'[^']*'`)

func maskSQL(s string) string {
	return rePassword.ReplaceAllString(s, "${1}'****'")
}

func printDryRun(target string, stmts []string) {
	fmt.Printf("[dry-run] -- %s\n", target)
	for _, s := range stmts {
		fmt.Printf("  %s;\n", maskSQL(s))
	}
}
