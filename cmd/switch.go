package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	mysqlpkg "github.com/master2sid2/replctl/internal/mysql"
)

var switchCmd = &cobra.Command{
	Use:   "switch",
	Short: "Configure GTID replication from source to destination",
	Long: `Creates a replication user on source, then runs STOP REPLICA / RESET REPLICA ALL /
CHANGE REPLICATION SOURCE TO ... SOURCE_AUTO_POSITION=1 / START REPLICA on destination.
Any replication already configured on destination is replaced, so the same command
repoints a replica from one source to another.
Run 'clone' first when the destination has no data yet.`,
	Example: `  # Basic replication setup
  replctl switch -s db1.example.com -d db2.example.com -u root \
    --repl-user replicator --repl-password replpass

  # Source and destination on different ports with different credentials
  replctl switch -s db1 -d db2 --src-port 3307 --dst-port 3308 \
    --src-user admin --dst-user dba --src-password p1 --dst-password p2 \
    --repl-user replicator --repl-password replpass

  # Source without TLS
  replctl switch -s db1 -d db2 --repl-ssl=false --repl-password replpass

  # Dry-run to preview SQL
  replctl switch -s db1 -d db2 --dry-run --repl-user replicator --repl-password replpass`,
	RunE: runSwitch,
}

var switchFlags struct {
	SSL bool
}

func init() {
	switchCmd.Flags().BoolVar(&switchFlags.SSL, "repl-ssl", true,
		"encrypt the replication connection (SOURCE_SSL=1); with --repl-ssl=false uses GET_SOURCE_PUBLIC_KEY=1 instead")
}

func runSwitch(cmd *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), Cfg.Timeout)
	defer cancel()

	dst, err := pairDst()
	if err != nil {
		return err
	}
	if Cfg.Demo {
		return demoSwitch(ctx, Cfg.Src, dst)
	}

	if err := promptCredentials(cmd, true, true); err != nil {
		return err
	}
	generated := ensurePassword(&Cfg.ReplPassword)

	srcIP, dstIP, err := addresses(dst)
	if err != nil {
		return err
	}

	var srcClient, dstClient *mysqlpkg.Client
	if !Cfg.DryRun {
		if srcClient, dstClient, err = connectPair(ctx, dst); err != nil {
			return err
		}
		defer srcClient.Close()
		defer dstClient.Close()
	}

	info := color.New(color.FgGreen)
	warn := color.New(color.FgYellow)

	info.Printf("Source:      %s (%s)\n", Cfg.Src, srcIP)
	info.Printf("Destination: %s (%s)\n", dst, dstIP)
	fmt.Println()

	if !Cfg.DryRun {
		// Everything is checked before the confirmation: once RESET REPLICA ALL has
		// run, the previous replication settings of the destination are gone.
		warnings, err := mysqlpkg.CheckReplicationPrereqs(ctx, srcClient, dstClient)
		if err != nil {
			return fmt.Errorf("pre-flight check: %w", err)
		}
		current, err := mysqlpkg.GetReplicaStatuses(ctx, dstClient)
		if err != nil {
			return err
		}
		for _, c := range current {
			warn.Printf("Current source of %s: %s:%s (IO=%s, SQL=%s) — will be removed\n",
				dst, c.SourceHost, c.SourcePort, c.IORunning, c.SQLRunning)
		}
		for _, w := range warnings {
			color.New(color.FgRed).Printf("  ! %s\n", w)
		}
	}
	warn.Printf("Replication: %s → %s\n", Cfg.Src, dst)
	fmt.Println()

	if !Cfg.DryRun {
		if err := confirm("Start replication?"); err != nil {
			return err
		}
	}

	// Step 1: Create replication user on source.
	info.Println("\n[1/2] Creating replication user on source...")
	if err := mysqlpkg.CreateReplUser(ctx, srcClient, dstIP, Cfg.ReplUser, Cfg.ReplPassword, Cfg.DryRun, Cfg.NativeAuth); err != nil {
		return fmt.Errorf("create repl user: %w", err)
	}
	if generated {
		announcePassword(dstIP)
	}

	// Step 2: Configure replication on destination.
	info.Println("[2/2] Configuring replication on destination...")
	if err := mysqlpkg.ConfigureReplication(ctx, dstClient, srcIP, srcPort(), Cfg.ReplUser, Cfg.ReplPassword, switchFlags.SSL, Cfg.DryRun); err != nil {
		return fmt.Errorf("configure replication: %w", err)
	}

	if Cfg.DryRun {
		info.Println("\nDry run: nothing was changed.")
		return nil
	}

	// Replication is only reported as working once both threads actually run.
	info.Println("\nWaiting for replication to start...")
	status, runErr := mysqlpkg.WaitReplicaRunning(ctx, dstClient, 30*time.Second)
	if status != nil {
		if Cfg.JSON {
			printLagJSON(os.Stdout, []lagResult{{host: dst, status: status}})
		} else {
			printReplicaStatus(status)
		}
	}
	return runErr
}

// announcePassword shows a generated replication password once, right after
// the account was created with it. JSON output keeps stdout machine-readable,
// so the notice goes to stderr then.
func announcePassword(dstIP string) {
	acct := fmt.Sprintf("'%s'@'%s'", Cfg.ReplUser, dstIP)
	w := io.Writer(os.Stdout)
	if Cfg.JSON {
		w = os.Stderr
	}
	note := color.New(color.FgYellow)
	if Cfg.DryRun {
		note.Fprintf(w, "  ! no --repl-password given: a random password will be generated for %s\n", acct)
		return
	}
	note.Fprintf(w, "  ! generated password for %s: %s\n", acct, Cfg.ReplPassword)
	note.Fprintln(w, "    replctl does not save it: keep it in a safe place (a password manager) if you need it later")
}
