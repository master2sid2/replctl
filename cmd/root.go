package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Cfg holds values resolved from flags + config file.
var Cfg struct {
	Src           string
	Dst           []string
	Port          int
	SrcPort       int
	DstPort       int
	User          string
	SrcUser       string
	DstUser       string
	Password      string
	SrcPassword   string
	DstPassword   string
	ClonePassword string
	ReplUser      string
	ReplPassword  string
	Timeout       time.Duration
	DryRun        bool
	JSON          bool
	Verbose       bool
	Demo          bool
}

func srcPort() int {
	if Cfg.SrcPort != 0 {
		return Cfg.SrcPort
	}
	return Cfg.Port
}

func dstPort() int {
	if Cfg.DstPort != 0 {
		return Cfg.DstPort
	}
	return Cfg.Port
}

func srcUser() string {
	if Cfg.SrcUser != "" {
		return Cfg.SrcUser
	}
	return Cfg.User
}

func dstUser() string {
	if Cfg.DstUser != "" {
		return Cfg.DstUser
	}
	return Cfg.User
}

func srcPassword() string {
	if Cfg.SrcPassword != "" {
		return Cfg.SrcPassword
	}
	return Cfg.Password
}

func dstPassword() string {
	if Cfg.DstPassword != "" {
		return Cfg.DstPassword
	}
	return Cfg.Password
}

func srcPort() int {
	if Cfg.SrcPort != 0 {
		return Cfg.SrcPort
	}
	return Cfg.Port
}

func dstPort() int {
	if Cfg.DstPort != 0 {
		return Cfg.DstPort
	}
	return Cfg.Port
}

func srcUser() string {
	if Cfg.SrcUser != "" {
		return Cfg.SrcUser
	}
	return Cfg.User
}

func dstUser() string {
	if Cfg.DstUser != "" {
		return Cfg.DstUser
	}
	return Cfg.User
}

func srcPassword() string {
	if Cfg.SrcPassword != "" {
		return Cfg.SrcPassword
	}
	return Cfg.Password
}

func dstPassword() string {
	if Cfg.DstPassword != "" {
		return Cfg.DstPassword
	}
	return Cfg.Password
}

var rootCmd = &cobra.Command{
	Use:   "mysqlctl",
	Short: "MySQL clone and replication management tool",
	Long: `mysqlctl manages MySQL cloning and replication.

Config file: ~/.mysqlctl.yaml
  port: 3306
  src_port: 0            # overrides port for source only
  dst_port: 0            # overrides port for destination only
  mysql_user: root
  src_user: ""           # overrides user for source only
  dst_user: ""           # overrides user for destination only
  mysql_password: ""
  src_password: ""       # overrides password for source only
  dst_password: ""       # overrides password for destination only
  clone_password: "changeme"
  repl_user: replicator
  repl_password: "changeme"
  timeout: 12h
`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute runs the root command.
func Execute() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)

	pf := rootCmd.PersistentFlags()

	pf.StringVarP(&Cfg.Src, "src", "s", "", "source MySQL host (IP or FQDN)")
	pf.StringArrayVarP(&Cfg.Dst, "dst", "d", nil, "destination host(s); repeat or comma-separate for multi-host lag")
	pf.IntVar(&Cfg.Port, "port", 3306, "MySQL port (default for both src and dst)")
	pf.IntVar(&Cfg.SrcPort, "src-port", 0, "source MySQL port (overrides --port)")
	pf.IntVar(&Cfg.DstPort, "dst-port", 0, "destination MySQL port (overrides --port)")
	pf.StringVarP(&Cfg.User, "user", "u", "root", "MySQL user (default for both src and dst)")
	pf.StringVar(&Cfg.SrcUser, "src-user", "", "source MySQL user (overrides --user)")
	pf.StringVar(&Cfg.DstUser, "dst-user", "", "destination MySQL user (overrides --user)")
	pf.StringVarP(&Cfg.Password, "password", "p", "", "MySQL password (default for both src and dst)")
	pf.StringVar(&Cfg.SrcPassword, "src-password", "", "source MySQL password (overrides --password)")
	pf.StringVar(&Cfg.DstPassword, "dst-password", "", "destination MySQL password (overrides --password)")
	pf.StringVar(&Cfg.ClonePassword, "clone-password", "", "clone_user password")
	pf.StringVar(&Cfg.ReplUser, "repl-user", "replicator", "replication user name")
	pf.StringVar(&Cfg.ReplPassword, "repl-password", "", "replication user password")
	pf.DurationVar(&Cfg.Timeout, "timeout", 12*time.Hour, "MySQL operation timeout")
	pf.BoolVar(&Cfg.DryRun, "dry-run", false, "print SQL without executing")
	pf.BoolVar(&Cfg.JSON, "json", false, "output results as JSON")
	pf.BoolVarP(&Cfg.Verbose, "verbose", "v", false, "enable debug logging")
	pf.BoolVar(&Cfg.Demo, "demo", false, "demo mode: simulate all operations without real MySQL")

	_ = viper.BindPFlag("port", pf.Lookup("port"))
	_ = viper.BindPFlag("mysql_user", pf.Lookup("user"))
	_ = viper.BindPFlag("src_user", pf.Lookup("src-user"))
	_ = viper.BindPFlag("dst_user", pf.Lookup("dst-user"))
	_ = viper.BindPFlag("mysql_password", pf.Lookup("password"))
	_ = viper.BindPFlag("src_password", pf.Lookup("src-password"))
	_ = viper.BindPFlag("dst_password", pf.Lookup("dst-password"))
	_ = viper.BindPFlag("clone_password", pf.Lookup("clone-password"))
	_ = viper.BindPFlag("repl_user", pf.Lookup("repl-user"))
	_ = viper.BindPFlag("repl_password", pf.Lookup("repl-password"))
	_ = viper.BindPFlag("timeout", pf.Lookup("timeout"))

	rootCmd.AddCommand(cloneCmd, switchCmd, lagCmd)
}

func initConfig() {
	viper.SetConfigName(".mysqlctl")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("$HOME")
	viper.AddConfigPath(".")
	viper.SetEnvPrefix("MYSQLCTL")
	viper.AutomaticEnv()

	_ = viper.ReadInConfig() // silently ignore missing config

	// Pull values from viper into Cfg (flags take priority, already bound above).
	if !rootCmd.PersistentFlags().Changed("port") {
		Cfg.Port = viper.GetInt("port")
	}
	if !rootCmd.PersistentFlags().Changed("user") {
		if u := viper.GetString("mysql_user"); u != "" {
			Cfg.User = u
		}
	}
	if !rootCmd.PersistentFlags().Changed("src-user") {
		if u := viper.GetString("src_user"); u != "" {
			Cfg.SrcUser = u
		}
	}
	if !rootCmd.PersistentFlags().Changed("dst-user") {
		if u := viper.GetString("dst_user"); u != "" {
			Cfg.DstUser = u
		}
	}
	if !rootCmd.PersistentFlags().Changed("password") {
		Cfg.Password = viper.GetString("mysql_password")
	}
	if !rootCmd.PersistentFlags().Changed("src-password") {
		Cfg.SrcPassword = viper.GetString("src_password")
	}
	if !rootCmd.PersistentFlags().Changed("dst-password") {
		Cfg.DstPassword = viper.GetString("dst_password")
	}
	if !rootCmd.PersistentFlags().Changed("clone-password") {
		Cfg.ClonePassword = viper.GetString("clone_password")
	}
	if !rootCmd.PersistentFlags().Changed("repl-user") {
		if u := viper.GetString("repl_user"); u != "" {
			Cfg.ReplUser = u
		}
	}
	if !rootCmd.PersistentFlags().Changed("repl-password") {
		Cfg.ReplPassword = viper.GetString("repl_password")
	}
	if !rootCmd.PersistentFlags().Changed("timeout") {
		if t := viper.GetDuration("timeout"); t > 0 {
			Cfg.Timeout = t
		}
	}

	level := slog.LevelInfo
	if Cfg.Verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}
