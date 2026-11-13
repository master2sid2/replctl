package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	mysqlpkg "github.com/master2sid2/replctl/internal/mysql"
)

var cloneCmd = &cobra.Command{
	Use:   "clone",
	Short: "Clone source database to destination using MySQL clone plugin",
	Long: `Installs the clone plugin on both servers, creates a temporary clone user on
the source, executes CLONE INSTANCE FROM on the destination (with live per-stage
progress), waits for destination MySQL to restart, verifies clone_status, then
cleans up all temporary objects.`,
	Example: `  # Basic clone using shared credentials
  replctl clone -s db1.example.com -d db2.example.com -u root --clone-password clonepass

  # Different ports on source and destination
  replctl clone -s db1 -d db2 --src-port 3307 --dst-port 3308 --clone-password clonepass

  # Different credentials on source and destination
  replctl clone -s db1 -d db2 --src-user admin --dst-user dba \
    --src-password srcpass --dst-password dstpass --clone-password clonepass

  # Servers talk over a private network the operator machine cannot see
  replctl clone -s db1.example.com -d db2.example.com --src-ip 10.0.0.1 --dst-ip 10.0.0.2

  # Dry-run to preview SQL without executing
  replctl clone -s db1 -d db2 --dry-run --clone-password clonepass

  # Custom timeout for large databases
  replctl clone -s db1 -d db2 --timeout 24h --clone-password clonepass`,
	RunE: runClone,
}

func runClone(cmd *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), Cfg.Timeout)
	defer cancel()

	dst, err := pairDst()
	if err != nil {
		return err
	}
	if Cfg.Demo {
		return demoClone(ctx, Cfg.Src, dst)
	}

	if err := promptCredentials(cmd, true, true); err != nil {
		return err
	}
	if err := requirePassword(&Cfg.ClonePassword, "Clone user password", "clone-password"); err != nil {
		return err
	}

	srcIP, dstIP, err := addresses(dst)
	if err != nil {
		return err
	}

	var srcClient, dstClient *mysqlpkg.Client
	if !Cfg.DryRun {
		if srcClient, dstClient, err = connectPair(ctx, dst); err != nil {
			return err
		}
		// dstClient is replaced after the restart, so close whatever is current on exit.
		defer func() {
			srcClient.Close()
			dstClient.Close()
		}()
	}

	warn := color.New(color.FgRed, color.Bold)
	info := color.New(color.FgGreen)

	info.Printf("Source:      %s (%s)\n", Cfg.Src, srcIP)
	info.Printf("Destination: %s (%s)\n", dst, dstIP)
	fmt.Println()
	if !Cfg.DryRun {
		if err := cloneInspect(ctx, srcClient, dstClient); err != nil {
			return err
		}
	}
	warn.Println("WARNING: ALL DATA ON DESTINATION WILL BE REPLACED!")
	fmt.Println()

	// cleanup runs once: explicitly on success, otherwise from the defer below.
	cleaned := false
	cleanup := func() bool {
		if cleaned {
			return true
		}
		cleaned = true
		return mysqlpkg.CleanupClone(srcClient, dstClient)
	}

	if !Cfg.DryRun {
		if err := confirm("Continue?"); err != nil {
			return err
		}
		// From here on the servers are modified: remove every clone artifact on
		// any exit path — failure, Ctrl+C and timeout included.
		defer cleanup()
	}

	// Step 1: Ensure clone plugin on both servers.
	info.Println("\n[1/4] Checking clone plugin...")
	if !Cfg.DryRun {
		if err := mysqlpkg.EnsureClonePlugin(ctx, srcClient); err != nil {
			return fmt.Errorf("clone plugin on src: %w", err)
		}
		if err := mysqlpkg.EnsureClonePlugin(ctx, dstClient); err != nil {
			return fmt.Errorf("clone plugin on dst: %w", err)
		}
	}

	// Step 2: Create the clone user on the donor.
	info.Println("[2/4] Creating clone user on source...")
	if err := mysqlpkg.CreateCloneUser(ctx, srcClient, srcIP, dstIP, Cfg.ClonePassword, Cfg.DryRun, Cfg.NativeAuth); err != nil {
		return err
	}

	// Step 3: Execute CLONE with per-stage progress display.
	// CLONE can run for hours, so it gets a dedicated connection with no read timeout;
	// dstClient keeps polling progress and kills the CLONE on Ctrl+C / --timeout.
	// RunClone returns when the CLONE statement finishes or the connection drops
	// (expected after MySQL auto-restart). Verification happens in step 4.
	info.Println("[3/4] Cloning...")
	totalStart := time.Now()
	var cloneClient *mysqlpkg.Client
	if !Cfg.DryRun {
		cloneClient = mysqlpkg.NewClient(mysqlpkg.Config{
			Host: dst, User: dstUser(), Password: dstPassword(),
			Port: dstPort(), Timeout: 30 * time.Second, ReadTimeout: -1,
		})
		defer cloneClient.Close()
	}
	since, err := mysqlpkg.RunClone(ctx, cloneClient, dstClient, srcIP, srcPort(), Cfg.ClonePassword, Cfg.DryRun)
	if err != nil {
		if ce, ok := errors.AsType[*mysqlpkg.CloneError](err); ok && !ce.DataDropped {
			info.Fprintln(os.Stderr, "Clone did not start copying: the destination still holds its previous data.")
		} else {
			warn.Fprintln(os.Stderr, "Clone did not complete: CLONE drops the existing data first, so the destination may now be empty or incomplete.")
		}
		return err
	}
	if Cfg.DryRun {
		info.Println("\nDry run: nothing was changed.")
		return nil
	}

	// Step 4: Wait for MySQL restart + verify clone completed on server side.
	// Success is reported only when clone_status shows that the clone started by
	// this run is Completed; a dropped connection is never treated as success.
	info.Println("[4/4] Waiting for destination MySQL to restart...")
	if err := mysqlpkg.WaitForMySQL(ctx, dst, dstPort(), dstUser(), dstPassword(), 5*time.Minute); err != nil {
		return err
	}

	// Re-open connection to the now-restarted dst.
	dstClient.Close()
	dstClient = mysqlpkg.NewClient(mysqlpkg.Config{
		Host: dst, User: dstUser(), Password: dstPassword(),
		Port: dstPort(), Timeout: 30 * time.Second,
	})

	info.Println("Verifying clone status...")
	if err := mysqlpkg.VerifyCloneStatus(ctx, dstClient, since, 2*time.Minute); err != nil {
		warn.Fprintln(os.Stderr, "Clone is NOT confirmed: do not use the destination until it is cloned again.")
		return err
	}

	total := time.Since(totalStart).Round(time.Second)
	info.Printf("Total time: %s\n", formatDuration(total))

	if !cleanup() {
		// The data is fine, but leftovers remain: make that visible in the exit code.
		return errors.New("clone completed, but cleanup failed")
	}

	info.Println("\nDone. Destination database is a fresh copy of source.")
	return nil
}

// cloneInspect refuses clones that cannot work and shows what is about to be
// destroyed on dst, plus source properties that change how dst behaves afterwards.
func cloneInspect(ctx context.Context, src, dst *mysqlpkg.Client) error {
	si, err := mysqlpkg.GetServerInfo(ctx, src)
	if err != nil {
		return err
	}
	di, err := mysqlpkg.GetServerInfo(ctx, dst)
	if err != nil {
		return err
	}
	if si.UUID == di.UUID {
		return fmt.Errorf("source and destination are the same MySQL instance (server_uuid %s)", si.UUID)
	}
	if err := mysqlpkg.CheckCloneVersions(si.Version, di.Version); err != nil {
		return err
	}

	note := color.New(color.FgYellow)

	if d, err := mysqlpkg.GetDataSummary(ctx, dst); err != nil {
		note.Printf("  ! could not inspect destination: %v\n", err)
	} else {
		fmt.Printf("Destination now holds %d schema(s), %s — it will be destroyed.\n", d.Schemas, mysqlpkg.FormatBytes(d.Bytes))
		if d.Replicas > 0 {
			note.Printf("  ! %d replica(s) are connected to the destination: it is acting as a replication source\n", d.Replicas)
		}
	}
	if rs, err := mysqlpkg.GetReplicaStatus(ctx, dst); err == nil && rs.IsConfigured {
		note.Printf("  ! destination is currently a replica of %s:%s\n", rs.SourceHost, rs.SourcePort)
	}

	if d, err := mysqlpkg.GetDataSummary(ctx, src); err != nil {
		note.Printf("  ! could not inspect source: %v\n", err)
	} else if d.EnabledEvents > 0 {
		note.Printf("  ! source has %d enabled event(s): after the clone they will also run on the destination (if event_scheduler is ON there)\n", d.EnabledEvents)
	}
	if rs, err := mysqlpkg.GetReplicaStatus(ctx, src); err == nil && rs.IsConfigured {
		note.Printf("  ! source is a replica of %s:%s: after the restart the destination will start replicating from it too (unless skip_replica_start is set)\n", rs.SourceHost, rs.SourcePort)
	}
	fmt.Println()
	return nil
}

func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%dm%ds", h, m, s)
	}
	return fmt.Sprintf("%dm%ds", m, s)
}
