package cmd

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/master2sid2/replctl/internal/dns"
	mysqlpkg "github.com/master2sid2/replctl/internal/mysql"
)

// Set through -ldflags by the Makefile; see buildInfo for the fallbacks.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// buildInfo returns the version, commit and build date. Values from the
// Makefile win; otherwise they come from what the Go toolchain recorded,
// which covers "go install ...@v0.1.0" and plain "go build" in a checkout.
func buildInfo() (v, c, d string) {
	v, c, d = version, commit, date
	if bi, ok := debug.ReadBuildInfo(); ok {
		// A tagged release ("v0.1.0"), not a pseudo-version of an untagged build.
		if mv := bi.Main.Version; v == "dev" && mv != "(devel)" && mv != "" && !strings.Contains(mv, "-") {
			v = strings.TrimPrefix(mv, "v")
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				c = cmp.Or(c, s.Value[:min(7, len(s.Value))])
			case "vcs.time":
				d = cmp.Or(d, s.Value[:min(10, len(s.Value))])
			}
		}
	}
	return v, cmp.Or(c, "none"), cmp.Or(d, "unknown")
}

func buildVersion() string {
	v, _, _ := buildInfo()
	return v
}

func versionTemplate() string {
	_, c, d := buildInfo()
	return "replctl {{.Version}}\n  commit : " + c + "\n  built  : " + d + "\n"
}

// errCancelled is returned when the operator does not confirm the operation.
var errCancelled = errors.New("cancelled by user, nothing was changed")

// stdin is shared by every prompt so that buffered input is never lost
// between them.
var stdin = bufio.NewReader(os.Stdin)

func stdinIsTerminal() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// promptPassword reads a password from the terminal without echoing.
// Returns an empty string without error when stdin is not a terminal.
func promptPassword(label string) (string, error) {
	if !stdinIsTerminal() {
		return "", nil
	}
	fmt.Fprintf(os.Stderr, "%s: ", label)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr) // newline after hidden input
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return string(b), nil
}

// promptLine reads a plain-text line from the terminal.
func promptLine(label string) (string, error) {
	if !stdinIsTerminal() {
		return "", nil
	}
	fmt.Fprintf(os.Stderr, "%s: ", label)
	line, err := stdin.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read input: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// promptCredentials asks for the shared user and password when the sides a
// command talks to (src, dst) still lack them. Per-host flags count: with
// --src-user and --dst-user both set, the shared user is never asked for.
func promptCredentials(cmd *cobra.Command, src, dst bool) error {
	userGiven := cmd.Root().PersistentFlags().Changed("user") || userSetFromConfig
	if !userGiven && ((src && Cfg.SrcUser == "") || (dst && Cfg.DstUser == "")) {
		val, err := promptLine(fmt.Sprintf("MySQL user (default: %s)", Cfg.User))
		if err != nil {
			return err
		}
		if val != "" {
			Cfg.User = val
		}
	}
	if (src && srcPassword() == "") || (dst && dstPassword() == "") {
		pass, err := promptPassword("MySQL password (" + Cfg.User + ")")
		if err != nil {
			return err
		}
		// An empty password is allowed (test instances).
		Cfg.Password = pass
	}
	return nil
}

// requirePassword prompts for a password if ptr points to an empty string and
// fails when none is given. flag is the CLI flag named in the error.
func requirePassword(ptr *string, label, flag string) error {
	if *ptr != "" {
		return nil
	}
	pass, err := promptPassword(label)
	if err != nil {
		return err
	}
	if pass == "" {
		return fmt.Errorf("password is required; use --%s or enter it when prompted", flag)
	}
	*ptr = pass
	return nil
}

// confirm asks a yes/no question unless --yes was given.
func confirm(question string) error {
	if Cfg.Yes {
		fmt.Println(question + " yes (--yes)")
		return nil
	}
	fmt.Print(question + " (Y/y to confirm): ")
	line, _ := stdin.ReadString('\n')
	if a := strings.TrimSpace(line); a != "Y" && a != "y" {
		return errCancelled
	}
	return nil
}

// pairDst validates --src and --dst for commands that work on one src → dst
// pair and returns the destination.
func pairDst() (string, error) {
	if Cfg.Src == "" {
		return "", errors.New("--src is required")
	}
	dst, err := singleDst()
	if err != nil {
		return "", err
	}
	if Cfg.Src == dst {
		return "", errors.New("source and destination must be different hosts")
	}
	return dst, nil
}

// singleDst returns the destination for commands that work on exactly one src → dst pair.
func singleDst() (string, error) {
	if len(Cfg.Dst) == 0 {
		return "", errors.New("--dst is required")
	}
	if len(Cfg.Dst) > 1 || strings.Contains(Cfg.Dst[0], ",") {
		return "", fmt.Errorf("this command works with exactly one destination, got %q", strings.Join(Cfg.Dst, ","))
	}
	return Cfg.Dst[0], nil
}

// splitHosts flattens repeated and comma-separated --dst values.
func splitHosts(values []string) []string {
	var hosts []string
	for _, v := range values {
		for h := range strings.SplitSeq(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hosts = append(hosts, h)
			}
		}
	}
	return hosts
}

// addresses returns the addresses the two servers use for each other: the
// source as the destination reaches it, and the destination as the source
// sees it. They default to the hosts resolved here, which is wrong when the
// servers talk over another network than this machine; --src-ip/--dst-ip fix that.
func addresses(dst string) (srcIP, dstIP string, err error) {
	var errs [2]error
	var wg sync.WaitGroup
	wg.Go(func() { srcIP, errs[0] = resolveOr(Cfg.SrcIP, Cfg.Src) })
	wg.Go(func() { dstIP, errs[1] = resolveOr(Cfg.DstIP, dst) })
	wg.Wait()
	if errs[0] != nil {
		return "", "", fmt.Errorf("resolve src: %w", errs[0])
	}
	if errs[1] != nil {
		return "", "", fmt.Errorf("resolve dst: %w", errs[1])
	}
	return srcIP, dstIP, nil
}

func resolveOr(override, host string) (string, error) {
	if override != "" {
		return override, nil
	}
	return dns.Resolve(host)
}

// connectPair opens and checks connections to both servers in parallel.
func connectPair(ctx context.Context, dst string) (src, dstC *mysqlpkg.Client, err error) {
	var errs [2]error
	var wg sync.WaitGroup
	wg.Go(func() { src, errs[0] = connect(ctx, Cfg.Src, srcPort(), srcUser(), srcPassword()) })
	wg.Go(func() { dstC, errs[1] = connect(ctx, dst, dstPort(), dstUser(), dstPassword()) })
	wg.Wait()
	if err := errors.Join(errs[:]...); err != nil {
		for _, c := range []*mysqlpkg.Client{src, dstC} {
			if c != nil {
				c.Close()
			}
		}
		return nil, nil, err
	}
	return src, dstC, nil
}

func connect(ctx context.Context, host string, port int, user, pass string) (*mysqlpkg.Client, error) {
	c := mysqlpkg.NewClient(mysqlpkg.Config{Host: host, Port: port, User: user, Password: pass, Timeout: 30 * time.Second})
	if err := c.Ping(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("connect to %s:%d: %w", host, port, err)
	}
	return c, nil
}
