// SPDX-FileCopyrightText: Copyright Hewlett Packard Enterprise Development LP
// SPDX-FileCopyrightText: Copyright (c) 2018 Made With MOXY Lda <hello@moxy.studio>
// SPDX-FileCopyrightText: Copyright (c) Kevin Mårtensson <kevinmartensson@gmail.com>
// SPDX-FileCopyrightText: Copyright (c) Sindre Sorhus <sindresorhus@gmail.com>
// SPDX-License-Identifier: MIT

// Ports packages/coding-agent/src/utils/child-process.ts (Windows spawnProcess routing through cross-spawn).
package crossspawn

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	// executableFile matches files CreateProcess starts directly
	// (cross-spawn lib/parse.js isExecutableRegExp).
	executableFile = regexp.MustCompile(`(?i)\.(?:com|exe)$`)
	// cmdShim matches npm's .cmd shims, whose arguments cmd.exe parses twice
	// (cross-spawn lib/parse.js isCmdShimRegExp).
	cmdShim = regexp.MustCompile(`(?i)node_modules[\\/].bin[\\/][^\\/]+\.cmd$`)
	// metaChars are the characters cmd.exe interprets (cross-spawn lib/util/escape.js metaCharsRegExp).
	metaChars = regexp.MustCompile("([()\\][%!^\"`<>&|;, *?])")
	// The lazy lookahead in cross-spawn 7.0.6 captures at most one backslash, not the whole run. Preserve its command-line bytes at both parse depths.
	backslashesQuote    = regexp.MustCompile(`(\\?)"`)
	trailingBackslashes = regexp.MustCompile(`(\\?)$`)
	shebangLine         = regexp.MustCompile(`^#![^\r\n\x{2028}\x{2029}]*`)
)

type windowsCommandPlan struct {
	name    string
	path    string
	args    []string
	cmdLine string
}

func planWindowsCommand(dir, name string, args []string) windowsCommandPlan {
	file := resolveCommand(dir, name)
	if shebang := readShebang(file); shebang != "" {
		args = append([]string{file}, args...)
		name = shebang
		file = resolveCommand(dir, name)
	}
	if executableFile.MatchString(file) {
		return windowsCommandPlan{name: name, path: file, args: args}
	}
	double := cmdShim.MatchString(file)
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, escapeCommand(filepath.Clean(name)))
	for _, arg := range args {
		parts = append(parts, escapeArgument(arg, double))
	}
	shell := os.Getenv("COMSPEC")
	if shell == "" {
		shell = "cmd.exe"
	}
	return windowsCommandPlan{name: shell, cmdLine: shell + ` /d /s /c "` + strings.Join(parts, " ") + `"`}
}

// resolveCommand includes cross-spawn's second lookup without PATHEXT, which
// finds extensionless shebang scripts on Windows.
func resolveCommand(dir, name string) string {
	if dir == "" {
		dir, _ = os.Getwd()
	} else {
		dir, _ = filepath.Abs(dir)
	}
	var paths []string
	switch {
	case filepath.IsAbs(name):
		paths = []string{name}
	case strings.ContainsAny(name, `:/\`):
		paths = []string{filepath.Join(dir, name)}
	default:
		paths = []string{filepath.Join(dir, name)}
		for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
			if !filepath.IsAbs(entry) {
				entry = filepath.Join(dir, entry)
			}
			paths = append(paths, filepath.Join(entry, name))
		}
	}
	for _, candidate := range paths {
		if file, err := exec.LookPath(candidate); err == nil {
			return file
		}
	}
	for _, file := range paths {
		if info, err := os.Stat(file); err == nil && !info.IsDir() {
			return file
		}
	}
	return ""
}

// readShebang mirrors cross-spawn's 150-byte read and shebang-command's literal
// space splitting. An interpreter argument stays part of the command name.
func readShebang(file string) string {
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	buffer := make([]byte, 150)
	_, _ = f.Read(buffer)
	line := shebangLine.FindString(string(buffer))
	if line == "" {
		return ""
	}
	line = strings.TrimPrefix(strings.TrimPrefix(line, "#!"), " ")
	parts := strings.Split(line, " ")
	pathParts := strings.Split(parts[0], "/")
	binary := pathParts[len(pathParts)-1]
	argument := ""
	if len(parts) > 1 {
		argument = parts[1]
	}
	if binary == "env" {
		return argument
	}
	if argument != "" {
		return binary + " " + argument
	}
	return binary
}

// escapeCommand is cross-spawn lib/util/escape.js escapeCommand.
func escapeCommand(command string) string {
	return metaChars.ReplaceAllString(command, "^$1")
}

// escapeArgument is cross-spawn lib/util/escape.js escapeArgument: quote the
// argument for the C runtime, then escape cmd.exe metacharacters (twice when
// cmd.exe parses the line a second time).
func escapeArgument(arg string, doubleEscape bool) string {
	arg = backslashesQuote.ReplaceAllString(arg, `$1$1\"`)
	arg = trailingBackslashes.ReplaceAllString(arg, `$1$1`)
	arg = `"` + arg + `"`
	arg = metaChars.ReplaceAllString(arg, "^$1")
	if doubleEscape {
		arg = metaChars.ReplaceAllString(arg, "^$1")
	}
	return arg
}
