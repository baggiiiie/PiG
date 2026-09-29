// Package resolvepath resolves user-supplied resource paths the way Pi's
// resolvePath does. It sits below internal/codingagent and coding/packagecontent
// so both share one implementation.
// Ports packages/coding-agent/src/utils/paths.ts:75-106.
package resolvepath

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unicode"

	"github.com/MichaelKinsy/PiG/internal/nodeurl"
)

// windowsShellDrivePath matches /c, /c/rest, /mnt/c/rest and /cygdrive/c/rest.
var windowsShellDrivePath = regexp.MustCompile(`(?i)^/(?:mnt/|cygdrive/)?([a-z])(?:/(.*))?$`)

// NormalizeWindowsShellPath converts a Git Bash, MSYS, Cygwin or WSL drive
// path to the form native Windows APIs accept.
//
// upstream: utils/paths.ts normalizeWindowsShellPath
func NormalizeWindowsShellPath(filePath string) string {
	if !strings.HasPrefix(filePath, "/") || strings.HasPrefix(filePath, "//") || strings.Contains(filePath, `\`) {
		return filePath
	}
	match := windowsShellDrivePath.FindStringSubmatch(filePath)
	if match == nil {
		return filePath
	}
	return strings.ToUpper(match[1]) + `:\` + strings.ReplaceAll(match[2], "/", `\`)
}

// Normalize applies Pi's default path normalization: a leading "~" or "~/"
// expands to the home directory and a file:// URL becomes its path through
// Node's fileURLToPath, whose errors it returns. An empty path stays empty.
func Normalize(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	windows := runtime.GOOS == "windows"
	if windows {
		path = NormalizeWindowsShellPath(path)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if path == "~" {
			return home, nil
		}
		if after, ok := strings.CutPrefix(path, "~/"); ok {
			return filepath.Join(home, after), nil
		}
		if after, ok := strings.CutPrefix(path, `~\`); ok && windows {
			return filepath.Join(home, after), nil
		}
	}
	if strings.HasPrefix(path, "file://") {
		return nodeurl.FileURLToPath(path, windows)
	}
	return path, nil
}

// Resolve normalizes input and baseDir, then resolves the input to an absolute
// path. An empty baseDir uses the process working directory.
func Resolve(input, baseDir string) (string, error) {
	normalized, err := Normalize(input)
	if err != nil {
		return "", err
	}
	normalizedBaseDir, err := Normalize(baseDir)
	if err != nil {
		return "", err
	}
	// Node's win32 isAbsolute accepts a rooted path without a drive, so Pi resolves it alone: it lands on the process working directory's drive, not the base's.
	if filepath.IsAbs(normalized) || runtime.GOOS == "windows" && (strings.HasPrefix(normalized, `/`) || strings.HasPrefix(normalized, `\`)) {
		return filepath.Abs(normalized)
	}
	base, err := filepath.Abs(normalizedBaseDir)
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		if volume := filepath.VolumeName(normalized); volume != "" {
			if !strings.EqualFold(volume, filepath.VolumeName(base)) {
				return filepath.Abs(normalized)
			}
			normalized = normalized[len(volume):]
		}
	}
	return filepath.Join(base, normalized), nil
}

// ResolveTrimmed is Resolve after String.prototype.trim, the { trim: true }
// option Pi's resource loader and package manager pass.
func ResolveTrimmed(input, baseDir string) (string, error) {
	return Resolve(strings.TrimFunc(input, isJSWhitespace), baseDir)
}

func isJSWhitespace(r rune) bool {
	switch r {
	case 0xFEFF:
		return true
	case 0x85:
		return false
	}
	return unicode.IsSpace(r)
}
