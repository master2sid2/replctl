package cmd

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/fatih/color"
	mysqlpkg "github.com/local/mysqlctl/internal/mysql"
	"github.com/local/mysqlctl/internal/progress"
)

const demoBanner = "\033[1;33m[DEMO MODE]\033[0m"

// demoClone simulates the full clone workflow with a fake progress bar.
func demoClone(ctx context.Context, src, dst string) error {
	info := color.New(color.FgGreen)
	warn := color.New(color.FgRed, color.Bold)
	yellow := color.New(color.FgYellow)

	fmt.Printf("%s  No real MySQL connections will be made.\n\n", demoBanner)

	info.Printf("Source:      %s (10.0.0.1)\n", src)
	info.Printf("Destination: %s (10.0.0.2)\n", dst)
	fmt.Println()
	warn.Println("WARNING: ALL DATA ON DESTINATION WILL BE REPLACED!")
	fmt.Println()
	fmt.Print("Continue? (Y/y to confirm): ")

	// In demo mode, auto-confirm after a short pause to look natural.
	time.Sleep(600 * time.Millisecond)
	fmt.Println("y")

	// Step 1: Clone plugin.
	info.Println("\n[1/4] Checking clone plugin...")
	demoStep(ctx, 400*time.Millisecond)
	yellow.Printf("  ✓ clone plugin active on %s\n", src)
	yellow.Printf("  ✓ clone plugin active on %s\n", dst)

	// Step 2: Users.
	info.Println("[2/4] Creating clone users...")
	demoStep(ctx, 500*time.Millisecond)
	yellow.Printf("  ✓ clone_user@10.0.0.2 created on %s (BACKUP_ADMIN)\n", src)
	yellow.Printf("  ✓ clone_user@10.0.0.1 created on %s (CLONE_ADMIN)\n", dst)

	// Step 3: Fake clone with progress bar.
	info.Println("[3/4] Cloning...")
	if err := demoRunClone(ctx); err != nil {
		return err
	}

	// Step 4: Fake restart.
	info.Println("[4/4] Waiting for destination MySQL to restart...")
	demoRestartWait(ctx, dst)

	// Cleanup.
	yellow.Println("\n  ✓ clone_user dropped on both servers")
	yellow.Println("  ✓ clone plugin uninstalled")

	info.Println("\nDone. Destination database is a fresh copy of source.")
	return nil
}

// demoRunClone shows a fake clone progress bar over 15–30 seconds.
func demoRunClone(ctx context.Context) error {
	totalDuration := time.Duration(15+rand.IntN(16)) * time.Second
	totalBytes := int64(40+rand.IntN(60)) * 1024 * 1024 * 1024 // 40–100 GB

	stages := []struct {
		name    string
		portion float64 // fraction of total time
	}{
		{"DROP DATA", 0.05},
		{"FILE COPY", 0.78},
		{"PAGE COPY", 0.08},
		{"REDO COPY", 0.05},
		{"FILE SYNC", 0.04},
	}

	bar := progress.NewSpinner("Cloning [DROP DATA]")
	bar.SetTotal(totalBytes)

	start := time.Now()
	stageStart := start
	stageIdx := 0
	var stageBytesStart int64

	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			bar.Finish()
			return ctx.Err()
		case <-ticker.C:
			elapsed := time.Since(start)
			if elapsed >= totalDuration {
				bar.SetCurrent(totalBytes)
				bar.Finish()
				elapsed = elapsed.Round(time.Second)
				color.New(color.FgGreen).Printf("Clone completed in %s\n", formatDuration(elapsed))
				return nil
			}

			// Advance through stages.
			stageFraction := stages[stageIdx].portion
			stageElapsed := time.Since(stageStart)
			stageDuration := time.Duration(float64(totalDuration) * stageFraction)

			if stageElapsed >= stageDuration && stageIdx < len(stages)-1 {
				stageIdx++
				stageStart = time.Now()
				stageBytesStart = int64(float64(totalBytes) * elapsedFraction(start, totalDuration))
			}

			current := int64(float64(totalBytes) * elapsedFraction(start, totalDuration))
			// Add jitter so the bar doesn't move perfectly linearly.
			jitter := rand.Int64N(int64(50*time.Millisecond)) - int64(25*time.Millisecond)
			current += jitter * int64(totalBytes) / int64(totalDuration)
			if current < stageBytesStart {
				current = stageBytesStart
			}
			if current > totalBytes {
				current = totalBytes
			}

			bar.SetDescription(fmt.Sprintf("Cloning [%s]", stages[stageIdx].name))
			bar.SetTotal(totalBytes)
			bar.SetCurrent(current)
		}
	}
}

func elapsedFraction(start time.Time, total time.Duration) float64 {
	f := time.Since(start).Seconds() / total.Seconds()
	if f > 1 {
		return 1
	}
	return f
}

// demoRestartWait simulates MySQL restart with countdown.
func demoRestartWait(ctx context.Context, host string) {
	yellow := color.New(color.FgYellow)
	steps := []struct {
		delay time.Duration
		msg   string
	}{
		{800 * time.Millisecond, "MySQL on " + host + " is shutting down..."},
		{1200 * time.Millisecond, "Waiting for restart..."},
		{1500 * time.Millisecond, "Waiting for restart... (2)"},
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

// demoSwitch simulates the replication setup.
func demoSwitch(ctx context.Context, src, dst string) error {
	info := color.New(color.FgGreen)
	yellow := color.New(color.FgYellow)

	fmt.Printf("%s  No real MySQL connections will be made.\n\n", demoBanner)

	info.Printf("Source:      %s (10.0.0.1)\n", src)
	info.Printf("Destination: %s (10.0.0.2)\n", dst)
	fmt.Println()
	color.New(color.FgYellow).Printf("Replication: %s → %s\n\n", src, dst)
	fmt.Print("Start replication? (Y/y to confirm): ")
	time.Sleep(500 * time.Millisecond)
	fmt.Println("y")

	info.Println("\n[1/2] Creating replication user on source...")
	demoStep(ctx, 600*time.Millisecond)
	yellow.Printf("  ✓ %s@10.0.0.2 created on %s (REPLICATION SLAVE)\n", Cfg.ReplUser, src)

	info.Println("[2/2] Configuring replication on destination...")
	demoStep(ctx, 800*time.Millisecond)
	yellow.Println("  ✓ STOP REPLICA")
	demoStep(ctx, 200*time.Millisecond)
	yellow.Println("  ✓ RESET REPLICA ALL")
	demoStep(ctx, 300*time.Millisecond)
	yellow.Printf("  ✓ CHANGE REPLICATION SOURCE TO SOURCE_HOST='10.0.0.1', SOURCE_AUTO_POSITION=1\n")
	demoStep(ctx, 200*time.Millisecond)
	yellow.Println("  ✓ START REPLICA")

	info.Println("\nWaiting 10s for replication to start...")
	demoStep(ctx, 2*time.Second) // only 2s in demo

	// Show fake healthy status.
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
		lag := rand.IntN(5) // 0–4 seconds lag
		s := &mysqlpkg.ReplicaStatus{
			Host:          h,
			SourceHost:    "master.demo.local",
			SourcePort:    strconv.Itoa(Cfg.Port),
			IORunning:     "Yes",
			SQLRunning:    "Yes",
			SecondsBehind: strconv.Itoa(lag),
			IOState:       "Waiting for source to send event",
			IsConfigured:  true,
		}
		results = append(results, lagResult{host: h, status: s})
	}

	if asJSON {
		printLagJSON(results)
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
	lag := make([]int, len(hosts))
	for {
		fmt.Print("\033[2J\033[H")
		fmt.Printf("%s  mysqlctl lag  —  %s  (interval: %s)\n\n",
			demoBanner, time.Now().Format("2006-01-02 15:04:05"), interval)

		var results []lagResult
		for i, h := range hosts {
			// Drift lag randomly: ±2 per tick, clamp 0–30.
			lag[i] += rand.IntN(5) - 2
			if lag[i] < 0 {
				lag[i] = 0
			}
			if lag[i] > 30 {
				lag[i] = 25
			}
			s := &mysqlpkg.ReplicaStatus{
				Host:          h,
				SourceHost:    "master.demo.local",
				SourcePort:    strconv.Itoa(Cfg.Port),
				IORunning:     "Yes",
				SQLRunning:    "Yes",
				SecondsBehind: strconv.Itoa(lag[i]),
				IOState:       "Waiting for source to send event",
				IsConfigured:  true,
			}
			results = append(results, lagResult{host: h, status: s})
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

func demoStep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
