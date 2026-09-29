//go:build windows

package crossspawn

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const argvHelper = "PIG_CROSSSPAWN_ARGV_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(argvHelper) == "1" {
		if err := json.NewEncoder(os.Stdout).Encode(os.Args[1:]); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestWindowsShellCommandNonCMD(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ComSpec", exe)
	line := `editor "file & name" %TOKEN%`
	cmd := ShellCommand(t.Context(), line)
	cmd.Env = append(os.Environ(), argvHelper+"=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"-c", line}) {
		t.Fatalf("shell args = %q", got)
	}
}

// Drive CreateProcess/cmd.exe as well as the portable planner. The command,
// cwd, script filename and arguments contain characters with shell meaning.
func TestWindowsCommandLiteralArguments(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "cwd & space")
	for _, dir := range []string{"bin", filepath.Join("node_modules", ".bin"), filepath.Join("node_modules", "Xbin")} {
		directory := filepath.Join(root, dir)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "receiver.exe"), data, 0o755); err != nil {
			t.Fatal(err)
		}
		writeExecutable(t, filepath.Join(directory, "shim & data.cmd"), "@\"%~dp0receiver.exe\" %*\r\n")
	}
	interpreter := filepath.Join(root, "bin", "receiver.exe")
	t.Setenv("PATH", filepath.Dir(interpreter)+string(os.PathListSeparator)+os.Getenv("PATH"))
	script := filepath.Join(root, "source & data.cmd")
	writeExecutable(t, script, "#!/usr/bin/env receiver.exe\r\n@echo INJECTED>sentinel\r\n")
	args := []string{"", "plain", "space in argument", "https://example.invalid/?a=1&echo.INJECTED>sentinel", "%TOKEN%", "!TOKEN!", "(a)|b^c", `quote"value`, `trailing\`}
	for _, name := range []string{interpreter, filepath.Join("bin", "shim & data.cmd"), filepath.Join("node_modules", ".bin", "shim & data.cmd"), filepath.Join("node_modules", "Xbin", "shim & data.cmd"), script} {
		t.Run(name, func(t *testing.T) {
			cmd := Command(t.Context(), root, name, args...)
			cmd.Env = append(os.Environ(), argvHelper+"=1", "TOKEN=EXPANDED")
			output, err := cmd.Output()
			if err != nil {
				t.Fatalf("run: %v; output %s", err, output)
			}
			var got []string
			if err := json.Unmarshal(output, &got); err != nil {
				t.Fatalf("decode argv: %v; output %s", err, output)
			}
			want := args
			if name == script {
				want = append([]string{script}, args...)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("argv = %q, want %q", got, want)
			}
			if _, err := os.Stat(filepath.Join(root, "sentinel")); !os.IsNotExist(err) {
				t.Fatalf("shell interpreted data: sentinel stat = %v", err)
			}
		})
	}
}
