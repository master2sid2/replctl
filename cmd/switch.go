package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/local/mysqlctl/internal/dns"
	mysqlpkg "github.com/local/mysqlctl/internal/mysql"
	"github.com/spf13/cobra"
)

var switchCmd = &cobra.Command{
	Use:   "switch",
	Short: "Configure GTID replication from source to destination",
	Long: `Creates a replication user on source, then runs STOP REPLICA / RESET REPLICA ALL /
CHANGE REPLICATION SOURCE TO ... SOURCE_AUTO_POSITION=1 / START REPLICA on destination.
Run 'clone' first to sync the data before using this command.`,
	Example: `  # Basic replication setup
  mysqlctl switch -s db1.example.com -d db2.example.com -u root -p secret \
    --repl-user replicator --repl-password replpass

  # Source and destination on different ports with different credentials
  mysqlctl switch -s db1 -d db2 --src-port 3307 --dst-port 3308 \
    --src-user admin --dst-user dba --src-password p1 --dst-password p2 \
    --repl-user replicator --repl-password replpass

  # Dry-run to preview SQL
  mysqlctl switch -s db1 -d db2 --dry-run --repl-user replicator --repl-password replpass`,
	RunE: runSwitch,
}

func runSwitch(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	if Cfg.Src == "" {
		return fmt.Errorf("--src is required")
	}
	if len(Cfg.Dst) == 0 {
		return fmt.Errorf("--dst is required")
	}
	dst := Cfg.Dst[0]

	if Cfg.Src == dst {
		return fmt.Errorf("source and destination must be different hosts")
	}

	if Cfg.Demo {
		return demoSwitch(ctx, Cfg.Src, dst)
	}

	// Prompt for credentials not set via flag or config.
	if err := askUser(&Cfg.User, cmd.Root().PersistentFlags().Changed("user")); err != nil {
		return err
	}
	if srcPassword() == "" || dstPassword() == "" {
		if err := askPassword(&Cfg.Password, "MySQL password ("+Cfg.User+")"); err != nil {
			return err
		}
	}
	if err := requirePassword(&Cfg.ReplPassword, "Replication user password", "repl-password"); err != nil {
		return err
	}

	var (
		srcIP, dstIP string
		resolveErr   [2]error
		wg           sync.WaitGroup
	)
	wg.Add(2)
	go func() { defer wg.Done(); srcIP, resolveErr[0] = dns.Resolve(Cfg.Src) }()
	go func() { defer wg.Done(); dstIP, resolveErr[1] = dns.Resolve(dst) }()
	wg.Wait()
	if resolveErr[0] != nil {
		return fmt.Errorf("resolve src: %w", resolveErr[0])
	}
	if resolveErr[1] != nil {
		return fmt.Errorf("resolve dst: %w", resolveErr[1])
	}

	var srcClient, dstClient *mysqlpkg.Client

	if !Cfg.DryRun {
		var connErr [2]error
		wg.Add(2)
		go func() {
			defer wg.Done()
			c, err := mysqlpkg.NewClient(mysqlpkg.Config{Host: Cfg.Src, User: srcUser(), Password: srcPassword(), Port: srcPort(), Timeout: 30 * time.Second})
			if err != nil {
				connErr[0] = err
				return
			}
			if err := c.Ping(ctx); err != nil {
				c.Close()
				connErr[0] = fmt.Errorf("ping %s: %w", Cfg.Src, err)
				return
			}
			srcClient = c
		}()
		go func() {
			defer wg.Done()
			c, err := mysqlpkg.NewClient(mysqlpkg.Config{Host: dst, User: dstUser(), Password: dstPassword(), Port: dstPort(), Timeout: 30 * time.Second})
			if err != nil {
				connErr[1] = err
				return
			}
			if err := c.Ping(ctx); err != nil {
				c.Close()
				connErr[1] = fmt.Errorf("ping %s: %w", dst, err)
				return
			}
			dstClient = c
		}()
		wg.Wait()
		if connErr[0] != nil {
			return connErr[0]
		}
		if connErr[1] != nil {
			return connErr[1]
		}
		defer srcClient.Close()
		defer dstClient.Close()
	}

	info := color.New(color.FgGreen)
	warn := color.New(color.FgYellow)

	info.Printf("Source:      %s (%s)\n", Cfg.Src, srcIP)
	info.Printf("Destination: %s (%s)\n", dst, dstIP)
	fmt.Println()
	warn.Printf("Replication: %s → %s\n", Cfg.Src, dst)
	fmt.Println()

	if !Cfg.DryRun {
		fmt.Print("Start replication? (Y/y to confirm): ")
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		if answer := strings.TrimSpace(scanner.Text()); answer != "Y" && answer != "y" {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	// Step 1: Create replication user on source.
	info.Println("\n[1/2] Creating replication user on source...")
	if err := mysqlpkg.CreateReplUser(ctx, srcClient, dstIP, Cfg.ReplUser, Cfg.ReplPassword, Cfg.DryRun); err != nil {
		return fmt.Errorf("create repl user: %w", err)
	}

	// Step 2: Configure replication on destination.
	info.Println("[2/2] Configuring replication on destination...")
	if err := mysqlpkg.ConfigureReplication(ctx, dstClient, srcIP, srcPort(), Cfg.ReplUser, Cfg.ReplPassword, Cfg.DryRun); err != nil {
		return fmt.Errorf("configure replication: %w", err)
	}

	if Cfg.DryRun {
		return nil
	}

	// Show status after a brief settling time.
	info.Println("\nWaiting 10s for replication to start...")
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timeAfter(10 * time.Second):
	}

	status, err := mysqlpkg.GetReplicaStatus(ctx, dstClient)
	if err != nil {
		return err
	}
	if Cfg.JSON {
		printLagJSON([]lagResult{{host: dst, status: status}})
	} else {
		printReplicaStatus(status)
	}
	return nil
}

// timeAfter is a small wrapper so it can be tested without actually sleeping.
func timeAfter(d time.Duration) <-chan time.Time {
	return time.After(d)
}
