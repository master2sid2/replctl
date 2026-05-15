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

var cloneCmd = &cobra.Command{
	Use:   "clone",
	Short: "Clone source database to destination using MySQL clone plugin",
	Long: `Installs the clone plugin on both servers, creates temporary clone users,
executes CLONE INSTANCE FROM (with live per-stage progress), waits for destination
MySQL to restart, verifies clone_status, then cleans up all temporary objects.`,
	Example: `  # Basic clone using shared credentials
  mysqlctl clone -s db1.example.com -d db2.example.com -u root -p secret --clone-password clonepass

  # Different ports on source and destination
  mysqlctl clone -s db1 -d db2 --src-port 3307 --dst-port 3308 --clone-password clonepass

  # Different credentials on source and destination
  mysqlctl clone -s db1 -d db2 --src-user admin --dst-user replicator \
    --src-password srcpass --dst-password dstpass --clone-password clonepass

  # Dry-run to preview SQL without executing
  mysqlctl clone -s db1 -d db2 --dry-run --clone-password clonepass

  # Custom timeout for large databases
  mysqlctl clone -s db1 -d db2 --timeout 24h --clone-password clonepass`,
	RunE: runClone,
}

func runClone(cmd *cobra.Command, _ []string) error {
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
		return demoClone(ctx, Cfg.Src, dst)
	}

	// Prompt for credentials not set via flag or config.
	if err := askUser(&Cfg.User, cmd.Root().PersistentFlags().Changed("user")); err != nil {
		return err
	}
	// Prompt for shared password only when needed (per-host passwords may cover both sides).
	if srcPassword() == "" || dstPassword() == "" {
		if err := askPassword(&Cfg.Password, "MySQL password ("+Cfg.User+")"); err != nil {
			return err
		}
	}
	if err := requirePassword(&Cfg.ClonePassword, "Clone user password", "clone-password"); err != nil {
		return err
	}

	// Resolve hostnames to IPs in parallel.
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
		// Check connectivity to both servers in parallel.
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
				connErr[0] = fmt.Errorf("ping %s: %w", Cfg.Src, err)
				c.Close()
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
				connErr[1] = fmt.Errorf("ping %s: %w", dst, err)
				c.Close()
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

	warn := color.New(color.FgRed, color.Bold)
	info := color.New(color.FgGreen)

	info.Printf("Source:      %s (%s)\n", Cfg.Src, srcIP)
	info.Printf("Destination: %s (%s)\n", dst, dstIP)
	fmt.Println()
	warn.Println("WARNING: ALL DATA ON DESTINATION WILL BE REPLACED!")
	fmt.Println()

	if !Cfg.DryRun {
		fmt.Print("Continue? (Y/y to confirm): ")
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		if answer := strings.TrimSpace(scanner.Text()); answer != "Y" && answer != "y" {
			fmt.Println("Cancelled.")
			return nil
		}
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

	// Step 2: Create clone users.
	info.Println("[2/4] Creating clone users...")
	if err := mysqlpkg.CreateCloneUsers(ctx, srcClient, dstClient, srcIP, dstIP, Cfg.ClonePassword, Cfg.DryRun, Cfg.NativeAuth); err != nil {
		return err
	}

	// Step 3: Execute CLONE with per-stage progress display.
	// CLONE can run for hours, so we use a dedicated connection with no read timeout.
	// RunClone returns when the CLONE statement finishes or the connection drops
	// (expected after MySQL auto-restart). Verification happens in step 4.
	info.Println("[3/4] Cloning...")
	totalStart := time.Now()
	if !Cfg.DryRun {
		cloneClient, err := mysqlpkg.NewClient(mysqlpkg.Config{
			Host: dst, User: dstUser(), Password: dstPassword(),
			Port: dstPort(), Timeout: 30 * time.Second, ReadTimeout: -1,
		})
		if err != nil {
			return fmt.Errorf("open clone connection: %w", err)
		}
		defer cloneClient.Close()
		dstClient = cloneClient
	}
	if err := mysqlpkg.RunClone(ctx, dstClient, srcIP, srcPort(), Cfg.ClonePassword, Cfg.DryRun); err != nil {
		if !Cfg.DryRun {
			mysqlpkg.CleanupClone(ctx, srcClient, dstClient, srcIP, dstIP)
		}
		return err
	}

	// Step 4: Wait for MySQL restart + verify clone completed on server side.
	// This is the key difference from the bash version: we confirm STATE='Completed'
	// so a dropped connection mid-clone is never silently treated as success.
	if !Cfg.DryRun {
		info.Println("[4/4] Waiting for destination MySQL to restart...")
		if err := mysqlpkg.WaitForMySQL(dst, dstPort(), dstUser(), dstPassword(), 5*time.Minute); err != nil {
			return err
		}

		// Re-open connection to the now-restarted dst.
		dstClient.Close()
		fresh, err := mysqlpkg.NewClient(mysqlpkg.Config{
			Host: dst, User: dstUser(), Password: dstPassword(),
			Port: dstPort(), Timeout: 30 * time.Second,
		})
		if err != nil {
			return fmt.Errorf("reconnect dst after restart: %w", err)
		}
		dstClient = fresh
		defer dstClient.Close()

		// Verify the clone actually completed (protects against dropped connections).
		info.Println("Verifying clone status...")
		if err := mysqlpkg.VerifyCloneStatus(ctx, dstClient); err != nil {
			return err
		}

		total := time.Since(totalStart).Round(time.Second)
		info.Printf("Total time: %s\n", formatDuration(total))

		mysqlpkg.CleanupClone(ctx, srcClient, dstClient, srcIP, dstIP)
	}

	info.Println("\nDone. Destination database is a fresh copy of source.")
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
