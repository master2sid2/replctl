package cmd

import (
	"fmt"
	"os"

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
