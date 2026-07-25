package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultHome is where the installer puts everything. It is the last place
// looked and the one that makes `madar -status` work from any directory on a
// standard install.
const DefaultHome = "/opt/madar"

// Discovery reports which paths were considered and which one was used, so a
// failure can say where it looked instead of naming one path the user never
// chose.
type Discovery struct {
	// ConfigPath is the file that was found. Empty when none was.
	ConfigPath string
	// EnvPath is the .env beside the config, or the explicit override.
	EnvPath string
	// Searched lists every candidate in the order it was tried.
	Searched []string
}

// DiscoverConfig resolves the configuration path.
//
// The order is: an explicit flag, then MADAR_CONFIG, then the working
// directory, then MADAR_HOME, then the XDG config directory. The working
// directory comes before the install location so a checkout under development
// keeps using its own config without anyone having to remember a flag.
//
// An explicit path is returned even when it does not exist: a caller who names
// a file wants to hear that the file is missing, not to silently get a
// different one.
func DiscoverConfig(explicit string) Discovery {
	if strings.TrimSpace(explicit) != "" {
		return Discovery{ConfigPath: explicit, Searched: []string{explicit}}
	}
	discovery := Discovery{}
	for _, candidate := range configCandidates() {
		if candidate == "" {
			continue
		}
		discovery.Searched = append(discovery.Searched, candidate)
		if discovery.ConfigPath != "" {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			discovery.ConfigPath = candidate
		}
	}
	return discovery
}

func configCandidates() []string {
	candidates := []string{
		strings.TrimSpace(os.Getenv("MADAR_CONFIG")),
		"config.yaml",
	}
	home := strings.TrimSpace(os.Getenv("MADAR_HOME"))
	if home == "" {
		home = DefaultHome
	}
	candidates = append(candidates, filepath.Join(home, "config.yaml"))
	if userConfig, err := os.UserConfigDir(); err == nil {
		candidates = append(candidates, filepath.Join(userConfig, "madar", "config.yaml"))
	}
	return candidates
}

// ResolveEnv returns the .env to load.
//
// Without an explicit path it sits beside the config that was actually found,
// not beside the working directory. Defaulting to ./.env is how a correct
// -config still ends up reporting a missing GITHUB_TOKEN: the config loads,
// the credentials do not, and the error names the wrong problem.
func ResolveEnv(explicit, configPath string) string {
	if strings.TrimSpace(explicit) != "" {
		return explicit
	}
	if strings.TrimSpace(configPath) == "" {
		return ".env"
	}
	return filepath.Join(filepath.Dir(configPath), ".env")
}

// NotFoundError explains where the configuration was looked for. Listing the
// candidates turns "it does not work" into "put the file here".
func (discovery Discovery) NotFoundError() error {
	var message strings.Builder
	message.WriteString("no config.yaml found. Looked in:")
	for _, candidate := range discovery.Searched {
		fmt.Fprintf(&message, "\n  %s", candidate)
	}
	message.WriteString(
		"\n\nPass -config <path>, set MADAR_CONFIG or MADAR_HOME, " +
			"or run the installer to create one.",
	)
	return fmt.Errorf("%s", message.String())
}
