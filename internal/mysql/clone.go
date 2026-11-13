package mysql

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fatih/color"
	gmysql "github.com/go-sql-driver/mysql"
	"golang.org/x/term"
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
		return "IDENTIFIED WITH mysql_native_password BY " + quote(password)
	}
	return "IDENTIFIED BY " + quote(password)
}

// CreateCloneUser creates clone_user@dstIP with BACKUP_ADMIN on the donor (src).
// The recipient needs no extra account: CLONE runs there as the operator's own
// user, which must have CLONE_ADMIN. A clone_user left over from an earlier run
// is dropped first so the password always matches.
func CreateCloneUser(ctx context.Context, src *Client, srcIP, dstIP, clonePass string, dryRun, nativeAuth bool) error {
	stmts := []string{
		"SET sql_log_bin=0",
		"DROP USER IF EXISTS " + account("clone_user", dstIP),
		fmt.Sprintf("CREATE USER %s %s", account("clone_user", dstIP), identifiedBy(clonePass, nativeAuth)),
		"GRANT BACKUP_ADMIN ON *.* TO " + account("clone_user", dstIP),
		"SET sql_log_bin=1",
	}
	if dryRun {
		printDryRun("SRC ("+srcIP+")", stmts)
		return nil
	}
	for _, s := range stmts {
		slog.Debug(maskSQL(s), "host", src.Host())
	}
	if err := src.ExecSession(ctx, stmts); err != nil {
		return fmt.Errorf("clone_user on src: %w", err)
	}
	color.New(color.FgYellow).Fprintf(os.Stderr, "  ✓ clone_user@%s created on %s (BACKUP_ADMIN)\n", dstIP, src.Host())
	return nil
}

// CloneError is returned by RunClone when the CLONE did not finish.
type CloneError struct {
	Err error
	// DataDropped is false only when it is known that the DROP DATA stage never
	// started, i.e. the destination still holds its previous data.
	DataDropped bool
}

func (e *CloneError) Error() string { return e.Err.Error() }
func (e *CloneError) Unwrap() error { return e.Err }

const (
	// erRestartFailed: CLONE copied the data but mysqld could not restart itself
	// because it is not managed by a supervisor (systemd, mysqld_safe).
	erRestartFailed = 3707
	erNoSuchTable   = 1146
	erPluginInUse   = 1883
)

// RunClone executes CLONE INSTANCE FROM on dst with per-stage progress display.
// mon is a regular client to the same server, used for progress polling and for
// killing the CLONE when ctx is cancelled or times out.
//
// It returns a server-side time marker taken right before the CLONE started; pass
// it to VerifyCloneStatus. A nil error only means the statement finished or the
// connection dropped (expected when MySQL restarts) — it is NOT proof of success.
func RunClone(ctx context.Context, dst, mon *Client, srcIP string, srcPort int, clonePass string, dryRun bool) (string, error) {
	donorSQL := "SET GLOBAL clone_valid_donor_list = " + quote(fmt.Sprintf("%s:%d", srcIP, srcPort))
	cloneSQL := fmt.Sprintf(
		"CLONE INSTANCE FROM %s:%d IDENTIFIED BY %s",
		account("clone_user", srcIP), srcPort, quote(clonePass),
	)

	if dryRun {
		printDryRun(fmt.Sprintf("DST (← %s:%d)", srcIP, srcPort), []string{donorSQL, cloneSQL})
		return "", nil
	}

	m, err := mon.QueryMap(ctx, "SELECT UNIX_TIMESTAMP(NOW(3)) AS ts")
	if err != nil {
		return "", fmt.Errorf("read server time on %s: %w", mon.Host(), err)
	}
	since := m["ts"]
	if _, err := strconv.ParseFloat(since, 64); err != nil {
		return "", fmt.Errorf("unexpected server time %q on %s", since, mon.Host())
	}

	// CLONE runs on one dedicated connection so we know which thread to KILL.
	conn, err := dst.db.Conn(ctx)
	if err != nil {
		return "", fmt.Errorf("open clone connection: %w", err)
	}
	defer conn.Close()

	var connID int64
	if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&connID); err != nil {
		return "", fmt.Errorf("read clone connection id: %w", err)
	}
	slog.Debug(donorSQL, "host", dst.Host())
	if _, err := conn.ExecContext(ctx, donorSQL); err != nil {
		return "", fmt.Errorf("set clone donor list: %w", err)
	}
	slog.Debug(maskSQL(cloneSQL), "host", dst.Host(), "connection_id", connID)

	cloneErrCh := make(chan error, 1)
	go func() {
		_, err := conn.ExecContext(ctx, cloneSQL)
		cloneErrCh <- err
	}()

	tracker := newCloneTracker()
	// clone_progress keeps the rows of the previous clone until a new one
	// reaches each stage, so only stages begun by this run are looked at.
	progressQuery := cloneProgressQuery + " WHERE BEGIN_TIME >= FROM_UNIXTIME(" + since + ")"
	poll := func() {
		pctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if rows, err := mon.QueryRows(pctx, progressQuery); err == nil {
			tracker.update(rows)
		}
	}
	failed := func(err error) (string, error) {
		return since, &CloneError{Err: err, DataDropped: tracker.dataDropped()}
	}
	interrupted := func() (string, error) {
		tracker.clearLine()
		killClone(mon, connID)
		poll()
		state := "is left with INCOMPLETE data, re-run clone"
		if !tracker.dataDropped() {
			state = "still holds its previous data"
		}
		return failed(fmt.Errorf("clone interrupted (%w): destination %s %s", ctx.Err(), dst.Host(), state))
	}

	dataTicker := time.NewTicker(2 * time.Second)
	spinTicker := time.NewTicker(100 * time.Millisecond)
	defer dataTicker.Stop()
	defer spinTicker.Stop()

	for {
		select {
		case err := <-cloneErrCh:
			if err != nil && ctx.Err() != nil {
				return interrupted()
			}
			// Final refresh so we don't miss the last completed stage.
			poll()
			tracker.clearLine()

			if mysqlErr, ok := errors.AsType[*gmysql.MySQLError](err); ok {
				if mysqlErr.Number == erRestartFailed {
					// The data is in place; the clone completes once mysqld is started
					// again, so let the caller wait for it and verify as usual.
					color.New(color.FgYellow).Fprintf(os.Stderr,
						"  ! mysqld on %s has shut down to finish the clone but cannot restart by itself.\n"+
							"    If no service manager (systemd, Docker restart policy) starts it again, start it manually.\n",
						dst.Host())
					return since, nil
				}
				return failed(fmt.Errorf("clone MySQL error %d: %s", mysqlErr.Number, mysqlErr.Message))
			}
			if err != nil {
				// Usually MySQL restarting after the clone, but may also be a network
				// failure: the caller must run WaitForMySQL + VerifyCloneStatus.
				slog.Debug("clone connection closed", "detail", err.Error())
			}
			return since, nil

		case <-dataTicker.C:
			poll()

		case <-spinTicker.C:
			tracker.spin()

		case <-ctx.Done():
			// Give the CLONE statement a moment to notice the cancellation; its
			// error says nothing new, the thread is killed below either way.
			select {
			case err := <-cloneErrCh:
				slog.Debug("clone statement ended after cancellation", "err", err)
			case <-time.After(10 * time.Second):
			}
			return interrupted()
		}
	}
}

// killClone terminates the server thread running CLONE. Closing the client
// connection is not enough: the server keeps cloning until the thread is killed.
func killClone(mon *Client, connID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	kill := fmt.Sprintf("KILL %d", connID)
	slog.Debug(kill, "host", mon.Host())
	err := mon.ExecOne(ctx, kill)
	if mysqlErr, ok := errors.AsType[*gmysql.MySQLError](err); err == nil || (ok && mysqlErr.Number == 1094) { // 1094: unknown thread id
		color.New(color.FgYellow).Fprintf(os.Stderr, "  ✓ CLONE stopped on %s\n", mon.Host())
		return
	}
	color.New(color.FgRed).Fprintf(os.Stderr,
		"  ✗ could not stop CLONE on %s (thread %d): %v\n    check SHOW PROCESSLIST there and KILL it manually\n",
		mon.Host(), connID, err)
}

// VerifyCloneStatus checks performance_schema.clone_status on c and returns nil only
// when it holds a record of a clone that began at or after since (the marker returned
// by RunClone) and finished with STATE = 'Completed'. While the state is 'In Progress'
// (connection dropped but the clone is still running) it keeps polling. Query errors
// are retried for up to maxErrWait, since the server may still be restarting.
// Anything else — no record, a record of an earlier clone, an unknown state — is an error.
func VerifyCloneStatus(ctx context.Context, c *Client, since string, maxErrWait time.Duration) error {
	q := fmt.Sprintf(`SELECT STATE, ERROR_NO, ERROR_MESSAGE, SOURCE,
UNIX_TIMESTAMP(BEGIN_TIME) >= %s AS IS_CURRENT
FROM performance_schema.clone_status ORDER BY ID DESC LIMIT 1`, since)

	wait := func(d time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
			return nil
		}
	}

	yellow := color.New(color.FgYellow)
	var errSince time.Time
	announced, pluginTried := false, false
	for {
		m, err := c.QueryMap(ctx, q)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if mysqlErr, ok := errors.AsType[*gmysql.MySQLError](err); ok && mysqlErr.Number == erNoSuchTable && !pluginTried {
				// The plugin is not loaded after the restart (it was not registered in
				// the cloned mysql.plugin); load it to read the persisted status.
				pluginTried = true
				if err := EnsureClonePlugin(ctx, c); err != nil {
					return fmt.Errorf("cannot verify clone: %w", err)
				}
				continue
			}
			if errSince.IsZero() {
				errSince = time.Now()
			}
			if time.Since(errSince) > maxErrWait {
				return fmt.Errorf("cannot verify clone: reading clone_status on %s: %w", c.Host(), err)
			}
			slog.Debug("clone_status query failed, retrying", "err", err)
			if err := wait(3 * time.Second); err != nil {
				return err
			}
			continue
		}
		errSince = time.Time{}

		if m == nil {
			return fmt.Errorf("clone not confirmed: clone_status on %s is empty — the CLONE never started", c.Host())
		}
		if m["IS_CURRENT"] != "1" {
			return fmt.Errorf(
				"clone not confirmed: clone_status on %s only has an earlier clone (%s from %s) — this CLONE never started",
				c.Host(), m["STATE"], m["SOURCE"])
		}

		switch m["STATE"] {
		case "Completed":
			if no := m["ERROR_NO"]; no != "" && no != "0" {
				return fmt.Errorf("clone finished with error %s: %s", no, m["ERROR_MESSAGE"])
			}
			yellow.Fprintf(os.Stderr, "  ✓ STATE = Completed (source %s)\n", m["SOURCE"])
			return nil
		case "Failed":
			msg := m["ERROR_MESSAGE"]
			if msg == "" {
				msg = "error code " + m["ERROR_NO"]
			}
			return fmt.Errorf("clone failed on server: %s", msg)
		case "In Progress", "Not Started":
			if !announced {
				fmt.Fprintf(os.Stderr, "  clone still running on server, waiting...\n")
				announced = true
			}
			if err := wait(5 * time.Second); err != nil {
				return err
			}
		default:
			return fmt.Errorf("clone not confirmed: unexpected clone_status STATE %q on %s", m["STATE"], c.Host())
		}
	}
}

// WaitForMySQL polls host:port until MySQL accepts connections, with exponential backoff.
func WaitForMySQL(ctx context.Context, host string, port int, user, pass string, maxWait time.Duration) error {
	deadline := time.Now().Add(maxWait)
	delay := 3 * time.Second

	yellow := color.New(color.FgYellow)
	attempt := 0
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
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

		c := NewClient(Config{Host: host, Port: port, User: user, Password: pass, Timeout: 5 * time.Second})
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		pingErr := c.Ping(pctx)
		cancel()
		_ = c.Close()
		if pingErr == nil {
			yellow.Fprintf(os.Stderr, "  MySQL is back online ✓\n")
			return nil
		}
		slog.Debug("ping failed", "attempt", attempt, "err", pingErr)
	}
	return fmt.Errorf("MySQL on %s:%d did not come back within %s", host, port, maxWait)
}

// CleanupClone removes every clone artifact from both servers: all clone_user
// accounts (including the one that arrives on dst inside the cloned data) and the
// clone plugin. It uses its own context so it runs even after Ctrl+C or a timeout.
// Returns false when something could not be removed.
func CleanupClone(src, dst *Client) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ok := true
	for _, c := range []*Client{src, dst} {
		if err := cleanupCloneOn(ctx, c); err != nil {
			ok = false
			color.New(color.FgRed).Fprintf(os.Stderr, "  ✗ cleanup on %s failed: %v\n", c.Host(), err)
			continue
		}
		color.New(color.FgYellow).Fprintf(os.Stderr, "  ✓ clone_user dropped, clone plugin uninstalled on %s\n", c.Host())
	}
	if !ok {
		color.New(color.FgRed).Fprintf(os.Stderr,
			"  cleanup is incomplete: remove 'clone_user' and the clone plugin manually (run with -v for the SQL)\n")
	}
	return ok
}

func cleanupCloneOn(ctx context.Context, c *Client) error {
	users, err := c.QueryRows(ctx, "SELECT Host AS h FROM mysql.user WHERE User = 'clone_user'")
	if err != nil {
		return fmt.Errorf("list clone_user accounts: %w", err)
	}
	plugin, err := c.QueryMap(ctx, "SELECT PLUGIN_STATUS FROM INFORMATION_SCHEMA.PLUGINS WHERE PLUGIN_NAME='clone'")
	if err != nil {
		return fmt.Errorf("check clone plugin: %w", err)
	}

	stmts := []string{"SET sql_log_bin=0"}
	for _, u := range users {
		stmts = append(stmts, "DROP USER IF EXISTS "+account("clone_user", u["h"]))
	}
	if plugin != nil {
		stmts = append(stmts, "UNINSTALL PLUGIN clone")
	}
	stmts = append(stmts, "SET sql_log_bin=1")

	for _, s := range stmts {
		slog.Debug(s, "host", c.Host())
	}
	// Right after an interrupted clone the donor sessions are still winding down
	// and the plugin is reported as in use for a few seconds.
	for {
		err := c.ExecSession(ctx, stmts)
		if mysqlErr, ok := errors.AsType[*gmysql.MySQLError](err); !ok || mysqlErr.Number != erPluginInUse {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(2 * time.Second):
		}
	}
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
	polled    bool // at least one progress query succeeded
	dropped   bool // the DROP DATA stage has begun
	tty       bool // stderr is a terminal: animate in place
	lastLog   time.Time
}

func newCloneTracker() *cloneTracker {
	return &cloneTracker{
		printed:   make(map[string]bool),
		stageSeen: make(map[string]time.Time),
		tty:       term.IsTerminal(int(os.Stderr.Fd())),
		lastLog:   time.Now(), // first log line after a minute, once there is progress to show
	}
}

// dataDropped reports whether the destination may have lost its data. Without
// a single successful progress query nothing is known, so assume the worst.
func (t *cloneTracker) dataDropped() bool { return t.dropped || !t.polled }

// clearLine removes the spinner line on a terminal.
func (t *cloneTracker) clearLine() {
	if t.tty {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
}

// update processes a fresh snapshot: prints newly completed stages, then renders spinner.
func (t *cloneTracker) update(rows []map[string]string) {
	t.lastRows = rows
	t.polled = true
	green := color.New(color.FgGreen)

	for _, row := range rows {
		name := row["STAGE"]
		if name == "DROP DATA" && row["STATE"] != "Not Started" {
			t.dropped = true
		}
		if row["STATE"] == "Completed" && !t.printed[name] {
			t.printed[name] = true
			dur := parseStageDuration(row["BEGIN_TIME"], row["END_TIME"])
			bytes, _ := strconv.ParseInt(row["DATA"], 10, 64)

			t.clearLine()
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

		if t.tty {
			fmt.Fprintf(os.Stderr, "\r  %s %-12s  %-8s%s    ", spin, name, fmtDur(elapsed), extra)
		} else if time.Since(t.lastLog) >= time.Minute {
			// Logs get a plain line once a minute instead of an animation.
			t.lastLog = time.Now()
			fmt.Fprintf(os.Stderr, "  … %-12s  %-8s%s\n", name, fmtDur(elapsed), extra)
		}
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

// quote returns s as a single-quoted SQL string literal.
func quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `''`)
	return "'" + s + "'"
}

// account returns the 'user'@'host' form of an account name.
func account(user, host string) string {
	return quote(user) + "@" + quote(host)
}

// FormatBytes renders a byte count in human-readable units.
func FormatBytes(b int64) string { return fmtBytes(b) }

var rePassword = regexp.MustCompile(`(?i)(BY\s+|PASSWORD=)'(?:[^'\\]|''|\\.)*'`)

// maskSQL hides password literals; use it for anything printed or logged.
func maskSQL(s string) string {
	return rePassword.ReplaceAllString(s, "${1}'****'")
}

func printDryRun(target string, stmts []string) {
	fmt.Printf("[dry-run] -- %s\n", target)
	for _, s := range stmts {
		fmt.Printf("  %s;\n", maskSQL(s))
	}
}
