//go:build parity

package runner

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestTmuxEnvPrefixDropsAmbientAgentHomes(t *testing.T) {
	for _, key := range []string{"PIG_CODING_AGENT_DIR", "PI_CODING_AGENT_DIR", "PIG_HOME", "PI_HOME"} {
		t.Setenv(key, "operator-state")
	}
	command := exec.Command("sh", "-c", tmuxEnvPrefix("")+`printf '%s|%s|%s|%s' "${PIG_CODING_AGENT_DIR-unset}" "${PI_CODING_AGENT_DIR-unset}" "${PIG_HOME-unset}" "${PI_HOME-unset}"`)
	out, err := command.CombinedOutput()
	if err != nil || string(out) != "unset|unset|unset|unset" {
		t.Fatalf("terminal base environment = %q, error = %v", out, err)
	}
}

func TestHeadlessHomeOverridesKeepExplicitFixtureHomes(t *testing.T) {
	t.Setenv("PIG_CODING_AGENT_DIR", "operator-state")
	command := exec.Command("sh", "-c", `printf '%s|%s|%s' "${PIG_CODING_AGENT_DIR:-unset}" "$PIG_HOME" "$PI_CODING_AGENT_DIR"`)
	command.Env = append(os.Environ(), clearedAgentHomeEnv()...)
	command.Env = append(command.Env, "PIG_HOME=fixture-home", "PI_CODING_AGENT_DIR=fixture-agent")
	out, err := command.CombinedOutput()
	if err != nil || string(out) != "unset|fixture-home|fixture-agent" {
		t.Fatalf("headless environment = %q, error = %v", out, err)
	}
}

// Worker-level config overrides must not outrank a scenario's snapshotted home.
// Scenario overrides are appended explicitly by every driver after this base.
func TestHermeticEnvironDropsAmbientAgentHomes(t *testing.T) {
	keys := []string{"PIG_CODING_AGENT_DIR", "PI_CODING_AGENT_DIR", "PIG_HOME", "PI_HOME"}
	for _, key := range keys {
		t.Setenv(key, "operator-state-must-not-be-read")
	}
	t.Setenv("HARNESS_UNRELATED_ENV", "preserved")
	env := hermeticEnviron()
	for _, key := range keys {
		for _, entry := range env {
			if strings.HasPrefix(entry, key+"=") {
				t.Errorf("ambient %s escaped into driver environment", key)
			}
		}
	}
	found := false
	for _, entry := range env {
		if entry == "HARNESS_UNRELATED_ENV=preserved" {
			found = true
		}
	}
	if !found {
		t.Fatal("unrelated environment was stripped")
	}
}
