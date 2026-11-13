package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/spf13/viper"
)

// configDir is where replctl keeps its config: $XDG_CONFIG_HOME/replctl, or
// ~/.config/replctl. On Windows it is the usual per-user config directory.
func configDir() (string, error) {
	if runtime.GOOS == "windows" {
		dir, err := os.UserConfigDir()
		return filepath.Join(dir, "replctl"), err
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "replctl"), nil
	}
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".config", "replctl"), err
}

// configFile picks the config to read: --config when given, otherwise the
// default location, then ~/.mysqlctl.yaml from the tool's former name.
// It returns "" when there is no config at all, which is fine.
func configFile(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil // must exist: reading it reports the error
	}
	dir, err := configDir()
	if err != nil {
		return "", nil // no home directory: run on flags alone
	}
	candidates := []string{filepath.Join(dir, "config.yaml")}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".mysqlctl.yaml"))
	}
	for i, path := range candidates {
		switch _, err := os.Stat(path); {
		case err == nil:
			if i > 0 {
				fmt.Fprintf(os.Stderr, "note: reading legacy config %s; move it to %s\n", path, candidates[0])
			}
			return path, nil
		case !errors.Is(err, fs.ErrNotExist):
			return "", err
		}
	}
	return "", nil
}

// readConfig loads the config file into viper. A broken config is an error:
// silently falling back to defaults could point the tool at the wrong server.
func readConfig() error {
	path, err := configFile(Cfg.ConfigFile)
	if err != nil || path == "" {
		return err
	}
	viper.SetConfigFile(path)
	viper.SetConfigType("yaml")
	return viper.ReadInConfig()
}
