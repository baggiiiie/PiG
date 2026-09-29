package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	sourceref "github.com/MichaelKinsy/PiG/coding/source"
	"github.com/MichaelKinsy/PiG/internal/codingagent"
)

// Ports packages/coding-agent/src/core/package-manager.ts (getLocalGitUpdateTarget, ensureGitRef, cleanAndInstallGitDependencies, repairMissingGitDependencies).
type gitUpdateTarget struct {
	ref       string
	fetchArgs []string
}

func getLocalGitUpdateTarget(checkout string) (gitUpdateTarget, error) {
	upstream, err := runPackageCapture(checkout, "git", "rev-parse", "--abbrev-ref", "@{upstream}")
	if branch, ok := strings.CutPrefix(upstream, "origin/"); err == nil && ok && branch != "" {
		if _, err := runPackageCapture(checkout, "git", "rev-parse", "@{upstream}"); err == nil {
			return gitUpdateTarget{"@{upstream}", []string{"fetch", "--prune", "--no-tags", "origin", "+refs/heads/" + branch + ":refs/remotes/origin/" + branch}}, nil
		}
	}
	// upstream: packages/coding-agent/src/core/package-manager.ts:getLocalGitUpdateTarget
	_ = runPackageProcess(checkout, "git", "remote", "set-head", "origin", "-a")
	if _, err := runPackageCapture(checkout, "git", "rev-parse", "origin/HEAD"); err != nil {
		return gitUpdateTarget{}, err
	}
	symbolic, _ := runPackageCapture(checkout, "git", "symbolic-ref", "refs/remotes/origin/HEAD")
	branch := strings.TrimPrefix(symbolic, "refs/remotes/origin/")
	refspec := "+HEAD:refs/remotes/origin/HEAD"
	if branch != "" {
		refspec = "+refs/heads/" + branch + ":refs/remotes/origin/" + branch
	}
	return gitUpdateTarget{"origin/HEAD", []string{"fetch", "--prune", "--no-tags", "origin", refspec}}, nil
}

func gitUpdateMarkerPath(checkout string) string {
	return filepath.Join(filepath.Dir(checkout), "."+filepath.Base(checkout)+".pi-update-incomplete")
}

func ensureGitRef(checkout, packageRoot string, sm *codingagent.SettingsManager, target gitUpdateTarget) error {
	if err := runPackageProcess(checkout, "git", target.fetchArgs...); err != nil {
		return err
	}
	local, err := runPackageCapture(checkout, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	commitRef := target.ref + "^{commit}"
	remote, err := runPackageCapture(checkout, "git", "rev-parse", commitRef)
	if err != nil {
		return err
	}
	marker := gitUpdateMarkerPath(checkout)
	if local == remote {
		if _, err := os.Stat(marker); err == nil {
			return cleanAndInstallGitDependencies(checkout, packageRoot, marker, sm)
		}
		return repairMissingGitDependencies(packageRoot, sm)
	}
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		return err
	}
	if err := runPackageProcess(checkout, "git", "reset", "--hard", commitRef); err != nil {
		return err
	}
	return cleanAndInstallGitDependencies(checkout, packageRoot, marker, sm)
}

func installGitDependencies(packageRoot string, sm *codingagent.SettingsManager) error {
	if _, err := os.Stat(filepath.Join(packageRoot, "package.json")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	command := defaultNpmCommand(sm)
	args := append([]string{}, command[1:]...)
	args = append(args, getGitDependencyInstallArgs(sm)...)
	return runPackageProcess(packageRoot, command[0], args...)
}

func repairMissingGitDependencies(packageRoot string, sm *codingagent.SettingsManager) error {
	data, err := os.ReadFile(filepath.Join(packageRoot, "package.json"))
	if err != nil {
		return nil
	}
	var manifest struct {
		Dependencies map[string]json.RawMessage `json:"dependencies"`
	}
	if json.Unmarshal(data, &manifest) != nil {
		return nil
	}
	root := filepath.Join(packageRoot, "node_modules")
	for name := range manifest.Dependencies {
		path := filepath.Join(root, filepath.FromSlash(name))
		if !strings.HasPrefix(path, root+string(filepath.Separator)) {
			continue
		}
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return installGitDependencies(packageRoot, sm)
		}
	}
	return nil
}

func cleanAndInstallGitDependencies(checkout, packageRoot, marker string, sm *codingagent.SettingsManager) error {
	if err := runPackageProcess(checkout, "git", "clean", "-fdx"); err != nil {
		// upstream: packages/coding-agent/src/core/package-manager.ts:cleanAndInstallGitDependencies
		_ = repairMissingGitDependencies(packageRoot, sm)
		return err
	}
	if err := requireGitSubdirectoryWithinCheckout(checkout, packageRoot); err != nil {
		return err
	}
	if err := installGitDependencies(packageRoot, sm); err != nil {
		return err
	}
	return removeGitUpdateMarker(marker)
}

func removeGitUpdateMarker(marker string) error {
	err := os.Remove(marker)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// resolveTemporaryGitSource waits for an unpinned cache refresh and its progress callbacks before discovering resources. Offline cache misses contribute no resources; refresh failures report an error event and retain the existing checkout.
// Ports packages/coding-agent/src/core/package-manager.ts (resolvePackageSources, refreshTemporaryGitSource, getTemporaryDir).
func resolveTemporaryGitSource(sm *codingagent.SettingsManager, source string, progress ProgressCallback) (string, error) {
	ref, err := sourceref.Parse(source, sourceref.Options{Bare: sourceref.BareReject})
	if err != nil || ref.Kind != sourceref.KindGit {
		return "", fmt.Errorf("invalid Git package source: %s", source)
	}
	checkout, err := temporaryGitCheckoutPath(sm.AgentDir(), ref)
	if err != nil {
		return "", err
	}
	packageRoot := filepath.Join(checkout, filepath.FromSlash(ref.GitSubdir))
	_, statErr := os.Stat(checkout)
	switch {
	case errors.Is(statErr, os.ErrNotExist):
		if IsOfflineModeEnabled() {
			return "", nil
		}
		if err := installGitCheckout(sm, ref, checkout, packageRoot, ""); err != nil {
			return "", err
		}
	case statErr != nil:
		return "", statErr
	case ref.GitRef == "" && !IsOfflineModeEnabled():
		// upstream: packages/coding-agent/src/core/package-manager.ts:refreshTemporaryGitSource
		_ = withProgress(progress, "pull", source, fmt.Sprintf("Refreshing %s...", source), func() error {
			return installGitCheckout(sm, ref, checkout, packageRoot, "")
		})
	}
	if err := requireGitSubdirectoryWithinCheckout(checkout, packageRoot); err != nil {
		return "", err
	}
	return packageRoot, nil
}

// temporaryGitCheckoutPath is a temporary Git source's checkout below the agent temporary extension root; paths outside that root are refused.
// Ports packages/coding-agent/src/core/package-manager.ts (getGitInstallPath, getTemporaryDir).
func temporaryGitCheckoutPath(agentDir string, ref sourceref.Ref) (string, error) {
	root := filepath.Join(agentDir, "tmp", "extensions")
	relative, err := gitCheckoutRelative(runtime.GOOS, root, ref)
	if err != nil {
		return "", err
	}
	host, path, _ := strings.Cut(relative, string(filepath.Separator))
	digest := sha256.Sum256([]byte("git-" + ref.GitHost + "-" + ref.GitPath))
	return filepath.Join(root, "git-"+host, fmt.Sprintf("%x", digest)[:8], path), nil
}

func pruneEmptyGitParents(checkout, root string) error {
	if root == "" {
		return nil
	}
	for current := filepath.Dir(checkout); current != root && strings.HasPrefix(current, root+string(filepath.Separator)); current = filepath.Dir(current) {
		entries, err := os.ReadDir(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			break
		}
		if err := os.Remove(current); err != nil {
			break
		}
	}
	return nil
}
