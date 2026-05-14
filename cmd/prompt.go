package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// promptPassword reads a password from the terminal without echoing.
// Returns empty string without error if stdin is not a terminal (e.g. pipes).
func promptPassword(label string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
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

// requirePassword prompts for a password if ptr points to an empty string.
// flag is the CLI flag name shown in the error message.
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

// askPassword prompts for a password if ptr is empty, but does NOT error on empty input
// (used for optional passwords like the MySQL root password on test instances).
func askPassword(ptr *string, label string) error {
	if *ptr != "" {
		return nil
	}
	pass, err := promptPassword(label)
	if err != nil {
		return err
	}
	*ptr = pass
	return nil
}

// promptLine reads a plain-text line from the terminal.
func promptLine(label string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", nil
	}
	fmt.Fprintf(os.Stderr, "%s: ", label)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read input: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// askUser prompts for a MySQL username when flagChanged is false (--user not set on CLI).
// Keeps the current value if the user presses Enter without typing.
func askUser(ptr *string, flagChanged bool) error {
	if flagChanged {
		return nil
	}
	val, err := promptLine(fmt.Sprintf("MySQL user (default: %s)", *ptr))
	if err != nil {
		return err
	}
	if val != "" {
		*ptr = val
	}
	return nil
}
