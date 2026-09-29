package subprocess_test

import (
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/internal/testenv"
)

// Run the covering upstream tests unchanged except for module/asset locations. Node executes the actual vendored implementation, not a second test implementation of ScrollView.
func TestNodeVendoredTuiUpstreamTests(t *testing.T) {
	t.Parallel()
	dist, err := filepath.Abs("runtime-node/shims/pi-dist/pi-tui")
	if err != nil {
		t.Fatal(err)
	}
	moduleURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(dist) + "/"}).String()
	sourceImport := regexp.MustCompile(`\.\./src/([^"` + "`" + `]+)\.ts`)
	for _, name := range []string{"layout", "terminal-image", "terminal", "native-module-path", "native-platform", "native-clipboard-linux"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(filepath.Join("../../../..", ".upstream/current/packages/tui/test", name+".test.ts"))
			if err != nil {
				t.Fatal(err)
			}
			source := sourceImport.ReplaceAllString(string(data), moduleURL+"${1}.js")
			source = strings.ReplaceAll(source, "../native/", moduleURL+"native/")
			testDir := t.TempDir()
			if name == "native-clipboard-linux" {
				if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
					t.Skip("upstream native Linux clipboard test supports Linux amd64/arm64 only")
				}
				for _, command := range []string{"cc", "Xvfb", "xclip", "pkg-config"} {
					if _, err := exec.LookPath(command); err != nil {
						t.Fatalf("native clipboard qualification requires %s: %v", command, err)
					}
				}
				if out, err := exec.CommandContext(t.Context(), "pkg-config", "--exists", "xcb").CombinedOutput(); err != nil {
					t.Fatalf("native clipboard qualification requires xcb development files: %v: %s", err, out)
				}
				root := testDir
				testDir = filepath.Join(root, "test")
				fixtures := filepath.Join(testDir, "fixtures")
				if err := os.MkdirAll(fixtures, 0o700); err != nil {
					t.Fatal(err)
				}
				// The original C allocation/selection fixtures include ../../native; point that include at the actual shipped implementation.
				testenv.RequireDirectoryLink(t, filepath.Join(dist, "native"), filepath.Join(root, "native"))
				for _, fixture := range []string{"clipboard-reader.cjs", "clipboard-worker-test.c", "clipboard-worker-test.cjs", "clipboard-x11-test.c"} {
					data, err := os.ReadFile(filepath.Join("../../../..", ".upstream/v0.87.1/packages/tui/test/fixtures", fixture))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(fixtures, fixture), data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			testFile := filepath.Join(testDir, name+".test.mts")
			if err := os.WriteFile(testFile, []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			// These tests use erasable TypeScript. All supported Node versions
			// strip it natively; transform-types is removed in Node 26.
			args := []string{"--test", "--test-reporter=tap"}
			if name == "terminal-image" && runtime.GOOS == "windows" {
				// Pi's CI runs these on Linux only. imageFallback shows the
				// home-shortened path with the platform separator, as Pi
				// does on Windows (~\.pi\agent\shot.png); these two cases
				// assert the POSIX spelling ~/.pi/agent/shot.png.
				for _, posixOnly := range []string{
					"shortens home-prefixed absolute paths without hyperlinks",
					"wraps shortened absolute paths in OSC 8 file links when hyperlinks are enabled",
				} {
					args = append(args, "--test-skip-pattern="+regexp.QuoteMeta(posixOnly))
				}
			}
			cmd := exec.CommandContext(t.Context(), "node", append(args, testFile)...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("upstream %s: %v\n%s", name, err, out)
			}
			if name == "native-clipboard-linux" {
				// Pinned test sites: metadata1 + worker modes3 + setup methods2 + four selection cases + transfer errors2 + partial cleanup2 + partial timeout2.
				for _, summary := range []string{"# tests 16\n", "# pass 16\n", "# fail 0\n", "# cancelled 0\n", "# skipped 0\n"} {
					if !strings.Contains(string(out), summary) {
						t.Fatalf("native clipboard cases did not all execute (%q missing):\n%s", summary, out)
					}
				}
			}
		})
	}
}

func TestVendoredPiTuiContainsEveryRuntimeModuleAndNativeAsset(t *testing.T) {
	root := filepath.Join(pinnedPiPackages, "node_modules/@earendil-works/pi-tui")
	for _, dir := range []string{"dist", "native"} {
		if err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			if dir == "dist" && !strings.HasSuffix(path, ".js") {
				return nil
			}
			rel, err := filepath.Rel(filepath.Join(root, "dist"), path)
			if err != nil {
				return err
			}
			if dir == "native" {
				rel, err = filepath.Rel(root, path)
				if err != nil {
					return err
				}
			}
			_, err = os.Stat(filepath.Join("runtime-node/shims/pi-dist/pi-tui", rel))
			return err
		}); err != nil {
			t.Fatalf("incomplete Pi TUI %s: %v", dir, err)
		}
	}
}
