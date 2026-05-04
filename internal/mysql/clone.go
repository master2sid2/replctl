package mysql

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"

	gmysql "github.com/go-sql-driver/mysql"
	"github.com/local/mysqlctl/internal/progress"
)

// EnsureClonePlugin installs the clone plugin on c if it is not already active.
func EnsureClonePlugin(ctx context.Context, c *Client) error {
	m, err := c.QueryMap(ctx,
		"SELECT PLUGIN_STATUS FROM INFORMATION_SCHEMA.PLUGINS WHERE PLUGIN_NAME='clone'",
	)
	if err != nil {
		return fmt.Errorf("check clone plugin on %s: %w", c.Host(), err)
	}
	if m != nil && m["PLUGIN_STATUS"] == "ACTIVE" {
		slog.Info("clone plugin already active", "host", c.Host())
		return nil
	}
	slog.Info("installing clone plugin", "host", c.Host())
	return c.ExecOne(ctx, "INSTALL PLUGIN clone SONAME 'mysql_clone.so'")
}

// CreateCloneUsers creates clone_user on src (BACKUP_ADMIN) and dst (CLONE_ADMIN).
// sql_log_bin is disabled so the statements are not replicated.
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

	slog.Info("creating clone user on source", "host", src.Host())
	if err := src.ExecSession(ctx, srcStmts); err != nil {
		return fmt.Errorf("clone_user on src: %w", err)
	}
	slog.Info("creating clone user on destination", "host", dst.Host())
	if err := dst.ExecSession(ctx, dstStmts); err != nil {
		return fmt.Errorf("clone_user on dst: %w", err)
	}
	return nil
}

// RunClone executes CLONE INSTANCE FROM on dst while showing live progress.
// MySQL restarts automatically after the clone; a connection drop is treated as success.
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

	if err := dst.ExecOne(ctx, donorSQL); err != nil {
		return fmt.Errorf("set clone donor list: %w", err)
	}

	slog.Info("starting clone", "src", fmt.Sprintf("%s:%d", srcIP, srcPort), "dst", dst.Host())

	cloneErrCh := make(chan error, 1)
	go func() {
		_, err := dst.db.ExecContext(ctx, cloneSQL)
		cloneErrCh <- err
	}()

	// Give MySQL a moment to register the clone before we start polling.
	time.Sleep(2 * time.Second)

	bar := progress.NewSpinner("Cloning...")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case err := <-cloneErrCh:
			bar.Finish()
			if isExpectedCloneError(err) {
				slog.Info("clone statement returned (MySQL will restart)")
				return nil
			}
			return fmt.Errorf("clone: %w", err)

		case <-ticker.C:
			updateProgress(dst, bar)

		case <-ctx.Done():
			bar.Finish()
			return ctx.Err()
		}
	}
}

func updateProgress(dst *Client, bar *progress.Bar) {
	// Use a fresh background context so cancelling the main ctx doesn't stop polling.
	ctx := context.Background()
	rows, err := dst.QueryRows(ctx,
		"SELECT STAGE, STATE, ESTIMATE, DATA_ELAPSED FROM performance_schema.clone_progress",
	)
	if err != nil || len(rows) == 0 {
		return
	}

	var totalEst, totalElp int64
	var activeStage string
	for _, row := range rows {
		if row["STATE"] == "In Progress" {
			activeStage = row["STAGE"]
		}
		if v, err := strconv.ParseInt(row["ESTIMATE"], 10, 64); err == nil {
			totalEst += v
		}
		if v, err := strconv.ParseInt(row["DATA_ELAPSED"], 10, 64); err == nil {
			totalElp += v
		}
	}

	if activeStage != "" {
		bar.SetDescription(fmt.Sprintf("Cloning [%s]", activeStage))
	}
	if totalEst > 0 {
		bar.SetTotal(totalEst)
		bar.SetCurrent(totalElp)
	}
}

// WaitForMySQL polls host:port until MySQL accepts connections or maxWait is exceeded.
// Uses exponential backoff starting at 3s, capped at 30s.
func WaitForMySQL(host string, port int, user, pass string, maxWait time.Duration) error {
	deadline := time.Now().Add(maxWait)
	delay := 3 * time.Second

	slog.Info("waiting for MySQL to restart", "host", host, "port", port)
	for time.Now().Before(deadline) {
		time.Sleep(delay)
		if delay < 30*time.Second {
			delay = min(delay*2, 30*time.Second)
		}

		cfg := Config{Host: host, Port: port, User: user, Password: pass, Timeout: 5 * time.Second}
		c, err := NewClient(cfg)
		if err != nil {
			continue
		}
		pingErr := c.Ping(context.Background())
		_ = c.Close()
		if pingErr == nil {
			slog.Info("MySQL is back", "host", host)
			return nil
		}
	}
	return fmt.Errorf("MySQL on %s:%d did not come back within %s", host, port, maxWait)
}

// CleanupClone drops clone_user and uninstalls the clone plugin on both servers.
// Errors are logged but not returned — this is always best-effort.
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
	if err := src.ExecSession(ctx, srcStmts); err != nil {
		slog.Warn("cleanup on src", "err", err)
	}
	if err := dst.ExecSession(ctx, dstStmts); err != nil {
		slog.Warn("cleanup on dst", "err", err)
	}
	slog.Info("cleanup complete")
}

// isExpectedCloneError returns true when err is a network/connection error caused
// by MySQL restarting after a successful clone (not an actual MySQL error).
func isExpectedCloneError(err error) bool {
	if err == nil {
		return true
	}
	var mysqlErr *gmysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return false // genuine MySQL error — not expected
	}
	// io.EOF or net errors mean the connection was reset — MySQL restarted.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// Any other non-MySQL error is treated as an expected restart.
	return true
}

func printDryRun(target string, stmts []string) {
	fmt.Printf("[dry-run] -- %s\n", target)
	for _, s := range stmts {
		fmt.Printf("  %s;\n", s)
	}
}
