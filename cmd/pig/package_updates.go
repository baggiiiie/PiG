package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/internal/codingagent"
)

// Network timeout for update checks. Mirrors upstream NETWORK_TIMEOUT_MS
// (package-manager.ts).
const updateCheckNetworkTimeout = 10 * time.Second

// updateCheckConcurrency limits parallel npm/git remote queries.
const updateCheckConcurrency = 5

// IsOfflineModeEnabled returns true when the PIG_OFFLINE (or legacy
// PI_OFFLINE) env var is set to a truthy value. Mirrors upstream
// isOfflineModeEnabled (package-manager.ts:29).
func IsOfflineModeEnabled() bool {
	if value := os.Getenv("PI_OFFLINE"); value != "" {
		return value == "1" || strings.EqualFold(value, "true") || strings.EqualFold(value, "yes")
	}
	v := strings.ToLower(strings.TrimSpace(os.Getenv("PIG_OFFLINE")))
	return v == "1" || v == "true" || v == "yes"
}

// PackageUpdate describes an available update for a configured package.
// Mirrors upstream PackageUpdate (package-manager.ts).
type PackageUpdate struct {
	Source      string // configured source string
	DisplayName string // human-readable name (npm package name or git org/repo)
	Type        string // "npm" or "git"
	Scope       string // "user" or "project"
}

// CheckForAvailableUpdates queries npm registry and git remotes for updates to installed, unpinned packages with at most five joined workers. It returns a non-nil slice, including when offline or no updates are available.
// Mirrors upstream DefaultPackageManager.checkForAvailableUpdates (packages/coding-agent/src/core/package-manager.ts:1186-1253).
// User-package metadata uses managed storage (D79); trusted project packages use cwd.
func CheckForAvailableUpdates(cwd string, sm *codingagent.SettingsManager) []PackageUpdate {
	if IsOfflineModeEnabled() {
		return []PackageUpdate{}
	}
	pkgs := configuredPackagesForResolution(cwd, sm)
	if len(pkgs) == 0 {
		return []PackageUpdate{}
	}

	type result struct {
		update *PackageUpdate
	}

	results := make([]result, len(pkgs))
	check := func(idx int) {
		p := pkgs[idx]

		source := p.Source.Source
		installed := p.InstalledPath
		if installed == "" {
			return
		}
		kind := detectSourceKind(source)
		// Exact npm versions are fixed; tags and ranges remain eligible for metadata lookup.
		if kind == "npm" && isPinnedNpm(source) {
			return
		}
		switch kind {
		case "npm":
			ref, err := parseNpmInstallRef(source)
			if err != nil {
				return
			}
			if npmHasAvailableUpdate(cwd, sm, ref, installed, p.Scope == "project") {
				results[idx] = result{update: &PackageUpdate{
					Source:      source,
					DisplayName: ref.NPMName,
					Type:        "npm",
					Scope:       p.Scope,
				}}
			}
		case "git":
			ref, ok := parseGitPackageSource(source)
			if !ok || ref.pinned {
				return
			}
			if gitHasAvailableUpdate(installed) {
				results[idx] = result{update: &PackageUpdate{
					Source:      source,
					DisplayName: ref.host + "/" + ref.path,
					Type:        "git",
					Scope:       p.Scope,
				}}
			}
		}
	}
	// upstream: packages/coding-agent/src/core/package-manager.ts:runWithConcurrency
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(updateCheckConcurrency, len(pkgs)) {
		wg.Go(func() {
			for idx := range jobs {
				check(idx)
			}
		})
	}
	for idx := range pkgs {
		jobs <- idx
	}
	close(jobs)
	wg.Wait()

	updates := []PackageUpdate{}
	for _, r := range results {
		if r.update != nil {
			updates = append(updates, *r.update)
		}
	}
	return updates
}

// isPinnedNpm reports whether a source specifies an exact semantic version, rather than a mutable tag or range.
func isPinnedNpm(source string) bool {
	ref, err := parseNpmInstallRef(source)
	if err != nil {
		return false
	}
	return isExactNpmVersion(ref.NPMVer)
}

// parseNpmSpec splits "@scope/name@1.2.3" into ("@scope/name", "1.2.3").
// Mirrors upstream parseNpmSpec (package-manager.ts:1637).
func parseNpmSpec(spec string) (name, version string) {
	m := regexp.MustCompile(`^(@?[^@]+(?:/[^@]+)?)(?:@(.+))?$`).FindStringSubmatch(spec)
	if len(m) == 0 {
		return spec, ""
	}
	return m[1], m[2]
}

// gitHasAvailableUpdate compares local HEAD against remote HEAD via
// `git ls-remote`. Mirrors upstream gitHasAvailableUpdate.
func gitHasAvailableUpdate(installedPath string) bool {
	if IsOfflineModeEnabled() {
		return false
	}
	localCmd := exec.Command("git", "rev-parse", "HEAD")
	localCmd.Dir = installedPath
	localHead, err := runWithTimeout(localCmd, updateCheckNetworkTimeout)
	if err != nil {
		return false
	}
	remoteHead, err := getRemoteGitHead(installedPath)
	return err == nil && strings.TrimSpace(localHead) != strings.TrimSpace(remoteHead)
}

func getRemoteGitHead(installedPath string) (string, error) {
	if upstreamRef := getGitUpstreamRef(installedPath); upstreamRef != "" {
		out, err := runGitRemoteCommand(installedPath, "ls-remote", "origin", upstreamRef)
		if err != nil {
			return "", err
		}
		if match := regexp.MustCompile(`(?m)^([0-9a-f]{40})\s+`).FindStringSubmatch(out); len(match) > 1 {
			return match[1], nil
		}
	}
	out, err := runGitRemoteCommand(installedPath, "ls-remote", "origin", "HEAD")
	if err != nil {
		return "", err
	}
	if match := regexp.MustCompile(`(?m)^([0-9a-f]{40})\s+HEAD$`).FindStringSubmatch(out); len(match) > 1 {
		return match[1], nil
	}
	return "", fmt.Errorf("Failed to determine remote HEAD")
}

func getGitUpstreamRef(installedPath string) string {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "@{upstream}")
	cmd.Dir = installedPath
	out, err := runWithTimeout(cmd, updateCheckNetworkTimeout)
	if err != nil {
		return ""
	}
	branch, ok := strings.CutPrefix(strings.TrimSpace(out), "origin/")
	if !ok || branch == "" {
		return ""
	}
	return "refs/heads/" + branch
}

// runGitRemoteCommand disables interactive credential prompts only for this remote query.
func runGitRemoteCommand(installedPath string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = installedPath
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return runWithTimeout(cmd, updateCheckNetworkTimeout)
}

// runWithTimeout captures a command and waits for child/output cleanup before returning, including after a timeout.
// Mirrors packages/coding-agent/src/core/package-manager.ts:runCommandCapture.
func runWithTimeout(cmd *exec.Cmd, timeout time.Duration) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var err error
	select {
	case err = <-done:
	case <-timer.C:
		terminatePackageCapture(cmd.Process)
		<-done
		return "", fmt.Errorf("%s timed out after %dms", strings.Join(cmd.Args, " "), timeout.Milliseconds())
	}
	if err != nil {
		if cmd.ProcessState == nil {
			return "", err
		}
		diagnostic := stderr.String()
		if diagnostic == "" {
			diagnostic = stdout.String()
		}
		return "", fmt.Errorf("%s failed with %s: %s", strings.Join(cmd.Args, " "), packageCaptureExitStatus(cmd.ProcessState), diagnostic)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// FormatPackageUpdates formats a list of available updates for display.
// Mirrors upstream showPackageUpdateNotification's body.
func FormatPackageUpdates(updates []PackageUpdate) string {
	if len(updates) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Package Updates Available\n")
	b.WriteString("Package updates are available. Run `pig update`\n")
	b.WriteString("Packages:\n")
	for _, u := range updates {
		fmt.Fprintf(&b, "- %s\n", u.DisplayName)
	}
	return strings.TrimRight(b.String(), "\n")
}

// EnsureConfiguredPackagesInstalled reinstalls missing installations and npm packages whose local manifest does not satisfy the configured version or range. Offline mode reports these sources as missing without installing them.
//
// Returns the source strings that were reinstalled and the sources that remain unavailable because installation failed or offline mode prevented it.
// upstream: packages/coding-agent/src/core/package-manager.ts:resolvePackageSources
func EnsureConfiguredPackagesInstalled(cwd string, sm *codingagent.SettingsManager) (reinstalled, missing []string) {
	packages := configuredPackagesForResolution(cwd, sm)
	if IsOfflineModeEnabled() {
		for _, pkg := range packages {
			if configuredPackageNeedsInstall(pkg) {
				missing = append(missing, pkg.Source.Source)
			}
		}
		return nil, missing
	}
	for _, pkg := range packages {
		if !configuredPackageNeedsInstall(pkg) {
			continue
		}
		local := pkg.Scope == "project"
		source := pkg.Source
		if detectSourceKind(source.Source) == "local" {
			resolved, err := resolveLocalPackageRoot(settingsBaseDirForManager(sm, local), source.Source)
			if err != nil {
				missing = append(missing, source.Source)
				continue
			}
			source.Source = resolved
		}
		if err := installPackageArtifacts(cwd, sm, source, local, nil); err != nil {
			missing = append(missing, pkg.Source.Source)
			continue
		}
		reinstalled = append(reinstalled, pkg.Source.Source)
	}
	return reinstalled, missing
}

func configuredPackageNeedsInstall(pkg configuredPackage) bool {
	return pkg.InstalledPath == "" || !installedPackageMatchesConfiguredVersion(pkg)
}

// installedPackageMatchesConfiguredVersion checks configured and temporary npm packages with the same local manifest rule. Inherited deltas use the source that owns their installation.
func installedPackageMatchesConfiguredVersion(pkg configuredPackage) bool {
	sourceText := pkg.ResolvedSource
	if sourceText == "" {
		sourceText = pkg.Source.Source
	}
	if detectSourceKind(sourceText) != "npm" {
		return true
	}
	source, err := parseNpmInstallRef(sourceText)
	return err == nil && installedNpmMatchesConfiguredVersion(source, pkg.InstalledPath)
}
