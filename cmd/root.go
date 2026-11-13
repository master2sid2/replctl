package cmd

import (
	"cmp"
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

// userSetFromConfig is true when mysql_user came from the config file or environment.
// Used by promptCredentials to skip interactive prompting when a user is already provided.
var userSetFromConfig bool

// Cfg holds values resolved from flags + config file.
var Cfg struct {
	ConfigFile    string
	Src           string
	Dst           []string
	SrcIP         string
	DstIP         string
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
	Yes           bool
	JSON          bool
	Verbose       bool
	Demo          bool
	NativeAuth    bool
}

func srcPort() int { return cmp.Or(Cfg.SrcPort, Cfg.Port) }
func dstPort() int { return cmp.Or(Cfg.DstPort, Cfg.Port) }

func srcUser() string { return cmp.Or(Cfg.SrcUser, Cfg.User) }
func dstUser() string { return cmp.Or(Cfg.DstUser, Cfg.User) }

func srcPassword() string { return cmp.Or(Cfg.SrcPassword, Cfg.Password) }
func dstPassword() string { return cmp.Or(Cfg.DstPassword, Cfg.Password) }

var rootCmd = &cobra.Command{
	Use:   "replctl",
	Short: "Clone MySQL servers and set up or repoint replicas",
	Long: `replctl builds and maintains MySQL replicas: it clones a source onto a
destination with the clone plugin, sets up or repoints GTID replication, and
shows replication status across replicas.

Config file: ~/.config/replctl/config.yaml ($XDG_CONFIG_HOME is honoured),
or the file given with --config:
  port: 3306
  src_port: 0            # overrides port for source only
  dst_port: 0            # overrides port for destination only
  mysql_user: root
  src_user: ""           # overrides user for source only
  dst_user: ""           # overrides user for destination only
  mysql_password: ""
  src_password: ""       # overrides password for source only
  dst_password: ""       # overrides password for destination only
  clone_password: ""     # random if empty
  repl_user: replicator
  repl_password: ""      # generated and shown once if empty
  timeout: 12h

Environment variables use the REPLCTL_ prefix, e.g. REPLCTL_MYSQL_PASSWORD.
`,
	Version:       buildVersion(),
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute runs the root command.
func Execute() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// The first signal starts a graceful shutdown; restoring the default handlers
	// lets a second Ctrl+C terminate the process if that shutdown hangs.
	go func() {
		<-ctx.Done()
		stop()
	}()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)
	rootCmd.SetVersionTemplate(versionTemplate())

	pf := rootCmd.PersistentFlags()

	pf.StringVar(&Cfg.ConfigFile, "config", "", "config file (default ~/.config/replctl/config.yaml)")

	pf.StringVarP(&Cfg.Src, "src", "s", "", "source MySQL host (IP or FQDN)")
	pf.StringArrayVarP(&Cfg.Dst, "dst", "d", nil, "destination host(s); repeat or comma-separate for multi-host lag")
	pf.StringVar(&Cfg.SrcIP, "src-ip", "", "address the destination uses to reach the source (default: --src resolved here)")
	pf.StringVar(&Cfg.DstIP, "dst-ip", "", "address the source sees the destination connecting from (default: --dst resolved here)")
	pf.IntVar(&Cfg.Port, "port", 3306, "MySQL port (default for both src and dst)")
	pf.IntVar(&Cfg.SrcPort, "src-port", 0, "source MySQL port (overrides --port)")
	pf.IntVar(&Cfg.DstPort, "dst-port", 0, "destination MySQL port (overrides --port)")
	pf.StringVarP(&Cfg.User, "user", "u", "root", "MySQL user (default for both src and dst)")
	pf.StringVar(&Cfg.SrcUser, "src-user", "", "source MySQL user (overrides --user)")
	pf.StringVar(&Cfg.DstUser, "dst-user", "", "destination MySQL user (overrides --user)")
	pf.StringVarP(&Cfg.Password, "password", "p", "", "MySQL password (default for both src and dst)")
	pf.StringVar(&Cfg.SrcPassword, "src-password", "", "source MySQL password (overrides --password)")
	pf.StringVar(&Cfg.DstPassword, "dst-password", "", "destination MySQL password (overrides --password)")
	pf.StringVar(&Cfg.ClonePassword, "clone-password", "", "clone_user password (random if not set)")
	pf.StringVar(&Cfg.ReplUser, "repl-user", "replicator", "replication user name")
	pf.StringVar(&Cfg.ReplPassword, "repl-password", "", "replication user password (generated and shown once if not set)")
	pf.DurationVar(&Cfg.Timeout, "timeout", 12*time.Hour, "overall time limit for clone/switch; a running CLONE is killed when it expires")
	pf.BoolVar(&Cfg.DryRun, "dry-run", false, "print SQL without executing")
	pf.BoolVarP(&Cfg.Yes, "yes", "y", false, "do not ask for confirmation (for scripts)")
	pf.BoolVar(&Cfg.JSON, "json", false, "output results as JSON")
	pf.BoolVarP(&Cfg.Verbose, "verbose", "v", false, "enable debug logging")
	pf.BoolVar(&Cfg.Demo, "demo", false, "demo mode: simulate all operations without real MySQL")
	pf.BoolVar(&Cfg.NativeAuth, "native-auth", false,
		"create users WITH mysql_native_password (deprecated; disabled by default in 8.4, removed in 9.0)")

	for key, flag := range map[string]string{
		"port": "port", "src_port": "src-port", "dst_port": "dst-port",
		"mysql_user": "user", "src_user": "src-user", "dst_user": "dst-user",
		"mysql_password": "password", "src_password": "src-password", "dst_password": "dst-password",
		"clone_password": "clone-password", "repl_user": "repl-user", "repl_password": "repl-password",
		"timeout": "timeout",
	} {
		_ = viper.BindPFlag(key, pf.Lookup(flag))
	}

	rootCmd.AddCommand(cloneCmd, switchCmd, lagCmd)
}

func initConfig() {
	viper.SetEnvPrefix("REPLCTL")
	viper.AutomaticEnv()

	if err := readConfig(); err != nil {
		fmt.Fprintf(os.Stderr, "error: config file: %v\n", err)
		os.Exit(1)
	}

	// Pull values from viper into Cfg (flags take priority, already bound above).
	changed := rootCmd.PersistentFlags().Changed
	if !changed("port") {
		Cfg.Port = viper.GetInt("port")
	}
	if !changed("src-port") {
		Cfg.SrcPort = viper.GetInt("src_port")
	}
	if !changed("dst-port") {
		Cfg.DstPort = viper.GetInt("dst_port")
	}
	if !changed("user") {
		if u := viper.GetString("mysql_user"); u != "" {
			Cfg.User = u
			// GetString also returns the flag default, so check where the value came from.
			_, fromEnv := os.LookupEnv("REPLCTL_MYSQL_USER")
			userSetFromConfig = viper.InConfig("mysql_user") || fromEnv
		}
	}
	if !changed("src-user") {
		Cfg.SrcUser = viper.GetString("src_user")
	}
	if !changed("dst-user") {
		Cfg.DstUser = viper.GetString("dst_user")
	}
	if !changed("password") {
		Cfg.Password = viper.GetString("mysql_password")
	}
	if !changed("src-password") {
		Cfg.SrcPassword = viper.GetString("src_password")
	}
	if !changed("dst-password") {
		Cfg.DstPassword = viper.GetString("dst_password")
	}
	if !changed("clone-password") {
		Cfg.ClonePassword = viper.GetString("clone_password")
	}
	if !changed("repl-user") {
		if u := viper.GetString("repl_user"); u != "" {
			Cfg.ReplUser = u
		}
	}
	if !changed("repl-password") {
		Cfg.ReplPassword = viper.GetString("repl_password")
	}
	if !changed("timeout") {
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
