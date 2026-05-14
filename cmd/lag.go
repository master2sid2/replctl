package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
	mysqlpkg "github.com/local/mysqlctl/internal/mysql"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
)

var lagFlags struct {
	Watch    bool
	Interval time.Duration
}

var lagCmd = &cobra.Command{
	Use:   "lag",
	Short: "Show replication lag and status",
	Long: `Connects to one or more replica hosts and prints replication status.`,
	Example: `  # Single replica
  mysqlctl lag -d slave1.example.com -u root -p secret

  # Multiple replicas (repeat flag or comma-separate)
  mysqlctl lag -d slave1 -d slave2 -d slave3
  mysqlctl lag -d slave1,slave2,slave3

  # Replicas on a non-standard port with dedicated credentials
  mysqlctl lag -d slave1,slave2 --dst-port 3307 --dst-user monitor --dst-password monpass

  # Auto-refresh every 3 seconds
  mysqlctl lag -d slave1 --watch --interval 3s

  # JSON output for scripting
  mysqlctl lag -d slave1 --json`,
	RunE: runLag,
}

func init() {
	lagCmd.Flags().BoolVar(&lagFlags.Watch, "watch", false, "continuously refresh status")
	lagCmd.Flags().DurationVar(&lagFlags.Interval, "interval", 2*time.Second, "refresh interval for --watch")
}

func runLag(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	if len(Cfg.Dst) == 0 {
		return fmt.Errorf("--dst is required")
	}

	if err := askUser(&Cfg.User, cmd.Root().PersistentFlags().Changed("user")); err != nil {
		return err
	}
	if dstPassword() == "" {
		if err := askPassword(&Cfg.Password, "MySQL password ("+Cfg.User+")"); err != nil {
			return err
		}
	}

	// Flatten comma-separated values (e.g. -d "h1,h2,h3").
	var hosts []string
	for _, d := range Cfg.Dst {
		for _, h := range strings.Split(d, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hosts = append(hosts, h)
			}
		}
	}

	if Cfg.Demo {
		if lagFlags.Watch {
			demoLagWatch(ctx, hosts, lagFlags.Interval)
			return nil
		}
		demoLag(hosts, Cfg.JSON)
		return nil
	}

	if lagFlags.Watch {
		for {
			fmt.Print("\033[2J\033[H") // clear screen
			fmt.Printf("mysqlctl lag  —  %s  (interval: %s)\n\n",
				time.Now().Format("2006-01-02 15:04:05"), lagFlags.Interval)
			printLagForHosts(ctx, hosts)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(lagFlags.Interval):
			}
		}
	}

	printLagForHosts(ctx, hosts)
	return nil
}

type lagResult struct {
	host   string
	status *mysqlpkg.ReplicaStatus
	err    error
}

func printLagForHosts(ctx context.Context, hosts []string) {
	results := make([]lagResult, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(idx int, host string) {
			defer wg.Done()
			qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			c, err := mysqlpkg.NewClient(mysqlpkg.Config{
				Host:     host,
				Port:     dstPort(),
				User:     dstUser(),
				Password: dstPassword(),
				Timeout:  10 * time.Second,
			})
			if err != nil {
				results[idx] = lagResult{host: host, err: err}
				return
			}
			defer c.Close()
			s, err := mysqlpkg.GetReplicaStatus(qctx, c)
			results[idx] = lagResult{host: host, status: s, err: err}
		}(i, h)
	}
	wg.Wait()

	if Cfg.JSON {
		printLagJSON(results)
		return
	}

	if len(hosts) == 1 {
		r := results[0]
		if r.err != nil {
			color.New(color.FgRed).Fprintf(os.Stderr, "error querying %s: %v\n", r.host, r.err)
			return
		}
		printReplicaStatus(r.status)
		return
	}

	printLagTable(results)
}

func printReplicaStatus(s *mysqlpkg.ReplicaStatus) {
	green := color.New(color.FgGreen).SprintFunc()
	red := color.New(color.FgRed).SprintFunc()
	yellow := color.New(color.FgYellow).SprintFunc()

	fmt.Printf("\n  Host: %s\n", s.Host)

	if !s.IsConfigured {
		fmt.Printf("  %s\n", yellow("replication not configured"))
		return
	}

	ioColor := green
	if s.IORunning != "Yes" {
		ioColor = red
	}
	sqlColor := green
	if s.SQLRunning != "Yes" {
		sqlColor = red
	}

	var lagStr string
	lag := s.SecondsBehindInt()
	switch {
	case lag < 0:
		lagStr = red("NULL (stopped)")
	case lag == 0:
		lagStr = green("0")
	default:
		lagStr = yellow(strconv.Itoa(lag))
	}

	fmt.Printf("  %-28s %s:%s\n", "Source_Host:", s.SourceHost, s.SourcePort)
	fmt.Printf("  %-28s %s\n", "Replica_IO_Running:", ioColor(s.IORunning))
	fmt.Printf("  %-28s %s\n", "Replica_SQL_Running:", sqlColor(s.SQLRunning))
	fmt.Printf("  %-28s %s\n", "Seconds_Behind_Source:", lagStr)
	if s.IOState != "" {
		fmt.Printf("  %-28s %s\n", "IO_State:", s.IOState)
	}
	if s.LastError != "" {
		fmt.Printf("  %-28s %s\n", color.RedString("Last_Error:"), color.RedString(s.LastError))
	}
}

func printLagTable(results []lagResult) {
	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Host", "IO", "SQL", "Lag (s)", "Source", "Last Error"})
	table.SetHeaderColor(
		tablewriter.Colors{tablewriter.Bold},
		tablewriter.Colors{tablewriter.Bold},
		tablewriter.Colors{tablewriter.Bold},
		tablewriter.Colors{tablewriter.Bold},
		tablewriter.Colors{tablewriter.Bold},
		tablewriter.Colors{tablewriter.Bold},
	)
	table.SetBorder(true)
	table.SetRowLine(false)

	for _, r := range results {
		if r.err != nil {
			table.Append([]string{r.host, "ERR", "ERR", "-", "-", r.err.Error()})
			continue
		}
		s := r.status
		if s == nil || !s.IsConfigured {
			table.Append([]string{r.host, "-", "-", "-", "-", "not configured"})
			continue
		}
		lag := "-"
		if l := s.SecondsBehindInt(); l >= 0 {
			lag = strconv.Itoa(l)
		}
		table.Append([]string{s.Host, s.IORunning, s.SQLRunning, lag, s.SourceHost, s.LastError})
	}
	table.Render()
}

type lagJSON struct {
	Host          string `json:"host"`
	SourceHost    string `json:"source_host,omitempty"`
	IORunning     bool   `json:"io_running"`
	SQLRunning    bool   `json:"sql_running"`
	SecondsBehind int    `json:"seconds_behind"`
	IsConfigured  bool   `json:"configured"`
	LastError     string `json:"last_error,omitempty"`
	Error         string `json:"error,omitempty"`
}

func printLagJSON(results []lagResult) {
	out := make([]lagJSON, len(results))
	for i, r := range results {
		j := lagJSON{Host: r.host}
		if r.err != nil {
			j.Error = r.err.Error()
		} else if r.status != nil {
			s := r.status
			j.SourceHost = s.SourceHost
			j.IORunning = s.IORunningBool()
			j.SQLRunning = s.SQLRunningBool()
			j.SecondsBehind = s.SecondsBehindInt()
			j.IsConfigured = s.IsConfigured
			j.LastError = s.LastError
		}
		out[i] = j
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if len(out) == 1 {
		_ = enc.Encode(out[0])
	} else {
		_ = enc.Encode(out)
	}
}
