package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/fatih/color"
	mysqlpkg "github.com/master2sid2/replctl/internal/mysql"
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
	Long:  `Connects to one or more replica hosts and prints replication status.`,
	Example: `  # Single replica
  replctl lag -d slave1.example.com

  # Multiple replicas (repeat flag or comma-separate)
  replctl lag -d slave1 -d slave2 -d slave3
  replctl lag -d slave1,slave2,slave3

  # Replicas on a non-standard port with dedicated credentials
  replctl lag -d slave1,slave2 --dst-port 3307 --dst-user monitor --dst-password monpass

  # Auto-refresh every 3 seconds
  replctl lag -d slave1 --watch --interval 3s

  # JSON output for scripting
  replctl lag -d slave1 --json`,
	RunE: runLag,
}

func init() {
	lagCmd.Flags().BoolVar(&lagFlags.Watch, "watch", false, "continuously refresh status")
	lagCmd.Flags().DurationVar(&lagFlags.Interval, "interval", 2*time.Second, "refresh interval for --watch")
}

func runLag(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	hosts := splitHosts(Cfg.Dst)
	if len(hosts) == 0 {
		return fmt.Errorf("--dst is required")
	}

	if Cfg.Demo {
		if lagFlags.Watch {
			demoLagWatch(ctx, hosts, lagFlags.Interval)
			return nil
		}
		demoLag(hosts, Cfg.JSON)
		return nil
	}

	if err := promptCredentials(cmd, false, true); err != nil {
		return err
	}

	if lagFlags.Watch {
		for {
			fmt.Print("\033[2J\033[H") // clear screen
			fmt.Printf("replctl lag  —  %s  (interval: %s)\n\n",
				time.Now().Format("2006-01-02 15:04:05"), lagFlags.Interval)
			_ = printLagForHosts(ctx, hosts)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(lagFlags.Interval):
			}
		}
	}

	if failed := printLagForHosts(ctx, hosts); failed > 0 {
		return fmt.Errorf("could not query %d of %d host(s)", failed, len(hosts))
	}
	return nil
}

type lagResult struct {
	host   string
	status *mysqlpkg.ReplicaStatus
	err    error
}

// printLagForHosts queries all hosts in parallel and prints one entry per replication
// channel. It returns the number of hosts that could not be queried.
func printLagForHosts(ctx context.Context, hosts []string) int {
	perHost := make([][]lagResult, len(hosts))
	var wg sync.WaitGroup
	for idx, host := range hosts {
		wg.Go(func() {
			qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			c := mysqlpkg.NewClient(mysqlpkg.Config{
				Host:     host,
				Port:     dstPort(),
				User:     dstUser(),
				Password: dstPassword(),
				Timeout:  10 * time.Second,
			})
			defer c.Close()
			all, err := mysqlpkg.GetReplicaStatuses(qctx, c)
			switch {
			case err != nil:
				perHost[idx] = []lagResult{{host: host, err: err}}
			case len(all) == 0:
				perHost[idx] = []lagResult{{host: host, status: &mysqlpkg.ReplicaStatus{Host: host}}}
			default:
				for _, s := range all {
					perHost[idx] = append(perHost[idx], lagResult{host: host, status: s})
				}
			}
		})
	}
	wg.Wait()

	var results []lagResult
	failed := 0
	for _, rs := range perHost {
		if rs[0].err != nil {
			failed++
		}
		results = append(results, rs...)
	}

	if Cfg.JSON {
		printLagJSON(os.Stdout, results)
		return failed
	}

	if len(hosts) == 1 {
		for _, r := range results {
			if r.err != nil {
				color.New(color.FgRed).Fprintf(os.Stderr, "error querying %s: %v\n", r.host, r.err)
				continue
			}
			printReplicaStatus(r.status)
		}
		return failed
	}

	printLagTable(results)
	return failed
}

func printReplicaStatus(s *mysqlpkg.ReplicaStatus) {
	green := color.New(color.FgGreen).SprintFunc()
	red := color.New(color.FgRed).SprintFunc()
	yellow := color.New(color.FgYellow).SprintFunc()

	fmt.Printf("\n  Host: %s\n", s.Host)
	if s.Channel != "" {
		fmt.Printf("  Channel: %s\n", s.Channel)
	}

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
	if !color.NoColor { // respects NO_COLOR and non-terminal output
		bold := tablewriter.Colors{tablewriter.Bold}
		table.SetHeaderColor(bold, bold, bold, bold, bold, bold)
	}
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
		host := s.Host
		if s.Channel != "" {
			host += " [" + s.Channel + "]"
		}
		table.Append([]string{host, s.IORunning, s.SQLRunning, lag, s.SourceHost, s.LastError})
	}
	table.Render()
}

type lagJSON struct {
	Host          string `json:"host"`
	Channel       string `json:"channel,omitempty"`
	SourceHost    string `json:"source_host,omitempty"`
	IORunning     bool   `json:"io_running"`
	SQLRunning    bool   `json:"sql_running"`
	SecondsBehind *int   `json:"seconds_behind"` // null when unknown: stopped, not configured or unreachable
	IsConfigured  bool   `json:"configured"`
	LastError     string `json:"last_error,omitempty"`
	Error         string `json:"error,omitempty"`
}

func printLagJSON(w io.Writer, results []lagResult) {
	out := make([]lagJSON, len(results))
	for i, r := range results {
		j := lagJSON{Host: r.host}
		if r.err != nil {
			j.Error = r.err.Error()
		} else if r.status != nil {
			s := r.status
			j.Channel = s.Channel
			j.SourceHost = s.SourceHost
			j.IORunning = s.IORunningBool()
			j.SQLRunning = s.SQLRunningBool()
			if lag := s.SecondsBehindInt(); lag >= 0 {
				j.SecondsBehind = &lag
			}
			j.IsConfigured = s.IsConfigured
			j.LastError = s.LastError
		}
		out[i] = j
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if len(out) == 1 {
		_ = enc.Encode(out[0])
	} else {
		_ = enc.Encode(out)
	}
}
