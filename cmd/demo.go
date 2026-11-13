package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strconv"
	"time"

	"github.com/fatih/color"
	mysqlpkg "github.com/master2sid2/replctl/internal/mysql"
)

var demoBanner = color.New(color.FgYellow, color.Bold).Sprint("[DEMO MODE]")

// demoClone simulates the full clone workflow with per-stage progress display.
func demoClone(ctx context.Context, src, dst string) error {
	info := color.New(color.FgGreen)
	warn := color.New(color.FgRed, color.Bold)

	fmt.Printf("%s  No real MySQL connections will be made.\n\n", demoBanner)

	info.Printf("Source:      %s (10.0.0.1)\n", src)
	info.Printf("Destination: %s (10.0.0.2)\n", dst)
	fmt.Println()
	warn.Println("WARNING: ALL DATA ON DESTINATION WILL BE REPLACED!")
	fmt.Println()
	fmt.Print("Continue? (Y/y to confirm): ")
	demoSleep(ctx, 600*time.Millisecond)
	fmt.Println("y")

	info.Println("\n[1/4] Checking clone plugin...")
	demoSleep(ctx, 400*time.Millisecond)
	slog.Debug("[demo] SELECT PLUGIN_STATUS FROM INFORMATION_SCHEMA.PLUGINS WHERE PLUGIN_NAME='clone'", "host", src)
	color.New(color.FgYellow).Printf("  ✓ clone plugin active on %s\n", src)
	slog.Debug("[demo] SELECT PLUGIN_STATUS FROM INFORMATION_SCHEMA.PLUGINS WHERE PLUGIN_NAME='clone'", "host", dst)
	color.New(color.FgYellow).Printf("  ✓ clone plugin active on %s\n", dst)

	info.Println("[2/4] Creating clone user on source...")
	demoSleep(ctx, 500*time.Millisecond)
	slog.Debug("[demo] CREATE USER 'clone_user'@'10.0.0.2' ... GRANT BACKUP_ADMIN", "host", src)
	color.New(color.FgYellow).Printf("  ✓ clone_user@10.0.0.2 created on %s (BACKUP_ADMIN)\n", src)

	info.Println("[3/4] Cloning...")
	totalStart := time.Now()
	if err := demoRunClone(ctx); err != nil {
		return err
	}

	info.Println("[4/4] Waiting for destination MySQL to restart...")
	demoRestartWait(ctx, dst)

	info.Println("Verifying clone status...")
	demoSleep(ctx, 300*time.Millisecond)
	slog.Debug("[demo] SELECT STATE FROM performance_schema.clone_status ORDER BY ID DESC LIMIT 1")
	color.New(color.FgYellow).Println("  ✓ STATE = Completed")

	total := time.Since(totalStart).Round(time.Second)
	info.Printf("Total time: %s\n", formatDuration(total))

	slog.Debug("[demo] DROP USER 'clone_user'@'10.0.0.2'; UNINSTALL PLUGIN clone", "host", src)
	slog.Debug("[demo] DROP USER 'clone_user'@'10.0.0.1'; UNINSTALL PLUGIN clone", "host", dst)
	color.New(color.FgYellow).Println("\n  ✓ clone_user dropped on both servers")
	color.New(color.FgYellow).Println("  ✓ clone plugin uninstalled")

	info.Println("\nDone. Destination database is a fresh copy of source.")
	return nil
}

// demoRunClone animates each clone stage on its own line, then prints a summary.
func demoRunClone(ctx context.Context) error {
	totalDuration := time.Duration(15+rand.IntN(16)) * time.Second
	totalBytes := int64(40+rand.IntN(60)) * 1024 * 1024 * 1024 // 40–100 GB

	type stageSpec struct {
		name    string
		portion float64 // fraction of totalDuration
		hasData bool
	}
	stages := []stageSpec{
		{"DROP DATA", 0.05, false},
		{"FILE COPY", 0.78, true},
		{"PAGE COPY", 0.08, false},
		{"REDO COPY", 0.05, false},
		{"FILE SYNC", 0.04, false},
	}

	spinChars := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	spinIdx := 0
	green := color.New(color.FgGreen)

	for _, stage := range stages {
		stageDur := time.Duration(float64(totalDuration) * stage.portion)
		stageStart := time.Now()
		ticker := time.NewTicker(120 * time.Millisecond)

	stageLoop:
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				fmt.Fprintln(os.Stderr)
				return ctx.Err()

			case now := <-ticker.C:
				elapsed := now.Sub(stageStart)
				if elapsed >= stageDur {
					ticker.Stop()
					break stageLoop
				}

				spin := spinChars[spinIdx%len(spinChars)]
				spinIdx++

				extra := ""
				if stage.hasData {
					progress := float64(elapsed) / float64(stageDur)
					transferred := int64(float64(totalBytes) * progress)
					extra = fmt.Sprintf("  %s / %s  %.0f%%",
						demoBytesStr(transferred), demoBytesStr(totalBytes), progress*100)
				}
				elapsedStr := demoDurStr(elapsed.Round(time.Second))
				fmt.Fprintf(os.Stderr, "\r  %s %-12s  %-8s%s    ",
					spin, stage.name, elapsedStr, extra)
			}
		}

		// Print completed stage.
		dur := time.Since(stageStart).Round(time.Second)
		fmt.Fprintf(os.Stderr, "\r\033[K")

		var bytesVal int64
		if stage.hasData {
			bytesVal = totalBytes
			green.Fprintf(os.Stderr, "  ✓ %-12s  %-10s  %s\n",
				stage.name, demoDurStr(dur), demoBytesStr(bytesVal))
		} else {
			green.Fprintf(os.Stderr, "  ✓ %-12s  %s\n",
				stage.name, demoDurStr(dur))
		}

	}

	return nil
}

// demoRestartWait simulates MySQL restart sequence.
func demoRestartWait(ctx context.Context, host string) {
	yellow := color.New(color.FgYellow)
	steps := []struct {
		delay time.Duration
		msg   string
	}{
		{700 * time.Millisecond, "MySQL on " + host + " is shutting down..."},
		{1100 * time.Millisecond, "Waiting for restart..."},
		{1400 * time.Millisecond, "Waiting for restart... (2)"},
		{900 * time.Millisecond, "MySQL is back online ✓"},
	}
	for _, s := range steps {
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.delay):
			yellow.Printf("  %s\n", s.msg)
		}
	}
}

// demoSwitch simulates the replication setup workflow.
func demoSwitch(ctx context.Context, src, dst string) error {
	info := color.New(color.FgGreen)
	yellow := color.New(color.FgYellow)

	fmt.Printf("%s  No real MySQL connections will be made.\n\n", demoBanner)

	info.Printf("Source:      %s (10.0.0.1)\n", src)
	info.Printf("Destination: %s (10.0.0.2)\n", dst)
	fmt.Println()
	color.New(color.FgYellow).Printf("Replication: %s → %s\n\n", src, dst)
	fmt.Print("Start replication? (Y/y to confirm): ")
	demoSleep(ctx, 500*time.Millisecond)
	fmt.Println("y")

	info.Println("\n[1/2] Creating replication user on source...")
	demoSleep(ctx, 600*time.Millisecond)
	slog.Debug("[demo] CREATE USER '"+Cfg.ReplUser+"'@'10.0.0.2' ... GRANT REPLICATION SLAVE", "host", src)
	yellow.Printf("  ✓ %s@10.0.0.2 created on %s (REPLICATION SLAVE)\n", Cfg.ReplUser, src)

	info.Println("[2/2] Configuring replication on destination...")
	for _, step := range []string{
		"STOP REPLICA",
		"RESET REPLICA ALL",
		"CHANGE REPLICATION SOURCE TO SOURCE_HOST='10.0.0.1', SOURCE_AUTO_POSITION=1",
		"START REPLICA",
	} {
		demoSleep(ctx, 250*time.Millisecond)
		slog.Debug("[demo] "+step, "host", dst)
		yellow.Printf("  ✓ %s\n", step)
	}

	info.Println("\nWaiting 10s for replication to start...")
	demoSleep(ctx, 2*time.Second)

	fakeStatus := &mysqlpkg.ReplicaStatus{
		Host:          dst,
		SourceHost:    src,
		SourcePort:    strconv.Itoa(Cfg.Port),
		IORunning:     "Yes",
		SQLRunning:    "Yes",
		SecondsBehind: "0",
		IOState:       "Waiting for source to send event",
		IsConfigured:  true,
	}
	printReplicaStatus(fakeStatus)
	return nil
}

// demoLag shows fake replication status for each host.
func demoLag(hosts []string, asJSON bool) {
	fmt.Printf("%s  Showing simulated replica status.\n\n", demoBanner)

	var results []lagResult
	for _, h := range hosts {
		lag := rand.IntN(5)
		results = append(results, lagResult{
			host: h,
			status: &mysqlpkg.ReplicaStatus{
				Host:          h,
				SourceHost:    "master.demo.local",
				SourcePort:    strconv.Itoa(Cfg.Port),
				IORunning:     "Yes",
				SQLRunning:    "Yes",
				SecondsBehind: strconv.Itoa(lag),
				IOState:       "Waiting for source to send event",
				IsConfigured:  true,
			},
		})
	}

	if asJSON {
		printLagJSON(os.Stdout, results)
		return
	}
	if len(hosts) == 1 {
		printReplicaStatus(results[0].status)
		return
	}
	printLagTable(results)
}

// demoLagWatch runs demoLag in watch mode with simulated lag drift.
func demoLagWatch(ctx context.Context, hosts []string, interval time.Duration) {
	lags := make([]int, len(hosts))
	for {
		fmt.Print("\033[2J\033[H")
		fmt.Printf("%s  replctl lag  —  %s  (interval: %s)\n\n",
			demoBanner, time.Now().Format("2006-01-02 15:04:05"), interval)

		var results []lagResult
		for i, h := range hosts {
			lags[i] += rand.IntN(5) - 2
			if lags[i] < 0 {
				lags[i] = 0
			}
			if lags[i] > 30 {
				lags[i] = 25
			}
			results = append(results, lagResult{
				host: h,
				status: &mysqlpkg.ReplicaStatus{
					Host:          h,
					SourceHost:    "master.demo.local",
					SourcePort:    strconv.Itoa(Cfg.Port),
					IORunning:     "Yes",
					SQLRunning:    "Yes",
					SecondsBehind: strconv.Itoa(lags[i]),
					IOState:       "Waiting for source to send event",
					IsConfigured:  true,
				},
			})
		}

		if len(hosts) == 1 {
			printReplicaStatus(results[0].status)
		} else {
			printLagTable(results)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func demoSleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// demoDurStr formats a duration for demo display (same style as internal fmtDur).
func demoDurStr(d time.Duration) string {
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

// demoBytesStr formats bytes for demo display.
func demoBytesStr(b int64) string {
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
