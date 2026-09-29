package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	sourceref "github.com/MichaelKinsy/PiG/coding/source"
	"github.com/MichaelKinsy/PiG/internal/codingagent"
)

// Ports packages/coding-agent/src/core/package-manager.ts
// resolveCLIExtensionSource resolves temporary Packages before loading their extensions. Offline sources that need installation contribute no resources. Cached unpinned Git sources refresh before discovery; a failed refresh retains the cache, while an initial installation failure is surfaced.
func resolveCLIExtensionSource(cwd, agentDir string, sm *codingagent.SettingsManager, source string, progress ProgressCallback) (string, error) {
	kind := detectSourceKind(source)
	if kind == "local" {
		return resolveLocalPackageRoot(cwd, source)
	}
	if kind == "git" {
		return resolveTemporaryGitSource(sm, source, progress)
	}
	ref, err := sourceref.Parse(source, sourceref.Options{Bare: sourceref.BareReject})
	if err != nil {
		return "", err
	}
	root, err := temporaryPackagePath(agentDir, "npm", "")
	if err != nil {
		return "", err
	}
	packageRoot := npmPackagePath(root, ref)
	pkg := configuredPackage{Source: codingagent.PackageSource{Source: source}, InstalledPath: packageRoot}
	if !installedPackageMatchesConfiguredVersion(pkg) {
		if IsOfflineModeEnabled() {
			return "", nil
		}
		if err := ensureManagedPackageRoot(root); err != nil {
			return "", err
		}
		command := defaultNpmCommand(sm)
		args := append([]string{}, command[1:]...)
		args = append(args, npmInstallArgs(npmCommandName(command), ref.Locator, root, ref.NPMRegistry)...)
		if err := runPackageProcess("", command[0], args...); err != nil {
			return "", err
		}
	}
	return packageRoot, nil
}

func npmPackagePath(root string, source sourceref.Ref) string {
	return filepath.Join(root, "node_modules", filepath.FromSlash(source.NPMName))
}

func temporaryPackagePath(agentDir, prefix, suffix string) (string, error) {
	tempRoot := filepath.Join(agentDir, "tmp", "extensions")
	if err := os.MkdirAll(tempRoot, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(tempRoot, 0o700); err != nil {
		return "", err
	}
	root, err := resolveManagedPackagePath(tempRoot, prefix)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(prefix + "-" + suffix))
	return resolveManagedPackagePath(root, fmt.Sprintf("%x", digest[:4]), filepath.FromSlash(suffix))
}

// resolveManagedPackagePath applies Node's lexical resolve semantics before checking containment; an absolute component replaces the preceding root.
// Ports packages/coding-agent/src/core/package-manager.ts.
func resolveManagedPackagePath(root string, parts ...string) (string, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved := absoluteRoot
	for _, part := range parts {
		if filepath.IsAbs(part) {
			resolved = filepath.Clean(part)
		} else {
			resolved = filepath.Join(resolved, part)
		}
	}
	if !isWithin(resolved, absoluteRoot) {
		return "", fmt.Errorf("Refusing to use path outside package install root: %s", resolved)
	}
	return resolved, nil
}
