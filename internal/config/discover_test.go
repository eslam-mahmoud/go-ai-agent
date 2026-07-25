package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chdir moves into dir for the duration of the test.
func chdir(t *testing.T, dir string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
}

func writeConfig(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("project:\n  repo: owner/repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// An explicit path is returned even when it does not exist: someone who names
// a file wants to hear that the file is missing, not to silently get another.
func TestExplicitPathIsUsedVerbatim(t *testing.T) {
	discovery := DiscoverConfig("/nowhere/config.yaml")
	if discovery.ConfigPath != "/nowhere/config.yaml" {
		t.Fatalf("ConfigPath = %q", discovery.ConfigPath)
	}
}

func TestWorkingDirectoryWinsOverTheInstallLocation(t *testing.T) {
	local := t.TempDir()
	home := t.TempDir()
	writeConfig(t, local)
	writeConfig(t, home)
	t.Setenv("MADAR_HOME", home)
	chdir(t, local)

	discovery := DiscoverConfig("")
	if discovery.ConfigPath != "config.yaml" {
		t.Fatalf("ConfigPath = %q, want the working-directory config", discovery.ConfigPath)
	}
}

// The whole point: `madar -status` from anywhere on a standard install.
func TestInstallLocationIsFoundFromAnyDirectory(t *testing.T) {
	home := t.TempDir()
	expected := writeConfig(t, home)
	t.Setenv("MADAR_HOME", home)
	chdir(t, t.TempDir())

	discovery := DiscoverConfig("")
	if discovery.ConfigPath != expected {
		t.Fatalf("ConfigPath = %q, want %q", discovery.ConfigPath, expected)
	}
}

func TestMadarConfigOverridesEverything(t *testing.T) {
	local := t.TempDir()
	writeConfig(t, local)
	elsewhere := t.TempDir()
	expected := writeConfig(t, elsewhere)
	t.Setenv("MADAR_CONFIG", expected)
	chdir(t, local)

	if discovery := DiscoverConfig(""); discovery.ConfigPath != expected {
		t.Fatalf("ConfigPath = %q, want %q", discovery.ConfigPath, expected)
	}
}

// "It does not work" has to become "put the file here".
func TestNotFoundReportsEveryPlaceItLooked(t *testing.T) {
	t.Setenv("MADAR_HOME", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("MADAR_CONFIG", "")
	chdir(t, t.TempDir())

	discovery := DiscoverConfig("")
	if discovery.ConfigPath != "" {
		t.Fatalf("ConfigPath = %q, want none", discovery.ConfigPath)
	}
	if len(discovery.Searched) < 2 {
		t.Fatalf("Searched = %v, want every candidate", discovery.Searched)
	}
	message := discovery.NotFoundError().Error()
	for _, candidate := range discovery.Searched {
		if !strings.Contains(message, candidate) {
			t.Fatalf("error does not name %q:\n%s", candidate, message)
		}
	}
	if !strings.Contains(message, "-config") {
		t.Fatalf("error does not say how to fix it:\n%s", message)
	}
}

// A correct -config with a .env resolved against the working directory is how
// a path problem gets reported as a missing credential.
func TestEnvResolvesBesideTheConfigThatWasFound(t *testing.T) {
	if got := ResolveEnv("", "/opt/madar/config.yaml"); got != "/opt/madar/.env" {
		t.Fatalf("EnvPath = %q, want the config's sibling", got)
	}
}

func TestExplicitEnvWins(t *testing.T) {
	if got := ResolveEnv("/tmp/other.env", "/opt/madar/config.yaml"); got != "/tmp/other.env" {
		t.Fatalf("EnvPath = %q", got)
	}
}

func TestEnvFallsBackWhenNoConfigWasFound(t *testing.T) {
	if got := ResolveEnv("", ""); got != ".env" {
		t.Fatalf("EnvPath = %q", got)
	}
}
