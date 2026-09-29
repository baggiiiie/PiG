//go:build windows

package codingagent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/MichaelKinsy/PiG/internal/testenv"
)

// Pi paths.ts:28-33 uses Node realpathSync, not realpathSync.native. libuv fs.c:284-303 rejects volume-GUID mount targets as symbolic links; Node therefore retains the mount's directory name instead of replacing it with a volume GUID.
func TestCanonicalizePathPreservesVolumeMountPoints(t *testing.T) {
	root := t.TempDir()
	drive := filepath.VolumeName(root) + `\`
	driveName, err := windows.UTF16PtrFromString(drive)
	if err != nil {
		t.Fatal(err)
	}
	var volume [windows.MAX_PATH + 1]uint16
	if err := windows.GetVolumeNameForVolumeMountPoint(driveName, &volume[0], uint32(len(volume))); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(target, "marker")
	if err := os.WriteFile(marker, []byte("mounted"), 0o600); err != nil {
		t.Fatal(err)
	}
	mount := filepath.Join(root, "mount")
	testenv.RequireDirectoryLink(t, windows.UTF16ToString(volume[:]), mount)
	t.Cleanup(func() {
		if err := os.Remove(mount); err != nil {
			t.Error(err)
		}
	})
	rel, err := filepath.Rel(drive, target)
	if err != nil {
		t.Fatal(err)
	}
	mountedMarker := filepath.Join(mount, rel, "marker")
	if data, err := os.ReadFile(mountedMarker); err != nil || string(data) != "mounted" {
		t.Fatalf("volume fixture content=%q error=%v", data, err)
	}
	alias := filepath.Join(root, "alias")
	testenv.RequireDirectoryLink(t, mount, alias)
	shortcut := filepath.Join(target, "shortcut")
	testenv.RequireDirectoryLink(t, root, shortcut)
	paths := []string{
		mount,
		mountedMarker,
		filepath.Join(alias, rel, "marker"),
		filepath.Join(mount, rel, "shortcut", "target", "marker"),
		filepath.Join(mount, rel, "missing"),
	}
	// Keep a native Node oracle in the regression rather than assuming that all reparse records have the same realpath semantics.
	out, err := exec.CommandContext(t.Context(), "node", append([]string{"-e", `const fs=require('node:fs'); console.log(JSON.stringify(process.argv.slice(1).map(p=>{try{return fs.realpathSync(p)}catch{return p}})))`}, paths...)...).Output()
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	if err := json.Unmarshal(out, &want); err != nil {
		t.Fatal(err)
	}
	if len(want) != len(paths) {
		t.Fatalf("oracle returned %q for %q", want, paths)
	}
	for i, path := range paths {
		for name, canonicalize := range map[string]func(string) string{
			"public": CanonicalizePath, "skills": canonicalizePath, "context": canonicalPath, "session": canonicalDir,
		} {
			if got := canonicalize(path); got != want[i] {
				t.Errorf("%s(%q)=%q; Node=%q", name, path, got, want[i])
			}
		}
	}
	// Ordinary alias paths merge, but a mount path and its drive path stay distinct in Pi's resource identity.
	got := DedupBySymlink([]string{mountedMarker, filepath.Join(alias, rel, "marker"), marker})
	if len(got) != 2 || got[0].Canonical != want[1] || got[1].Canonical != CanonicalizePath(marker) {
		t.Fatalf("resource identities=%+v; want mount and drive paths, with the alias merged", got)
	}
}

func TestVolumeGUIDPathClassification(t *testing.T) {
	for path, want := range map[string]bool{
		`\\?\Volume{1234}\`:           true,
		`\\?\volume{1234}\child`:      true,
		`C:\Volume{1234}`:             false,
		`\\?\C:\ordinary`:             false,
		`\\server\share\Volume{1234}`: false,
	} {
		if got := isVolumeGUIDPath(path); got != want {
			t.Errorf("isVolumeGUIDPath(%q)=%v; want %v", path, got, want)
		}
	}
}

// Node's realpathSync resolves each link target with path.resolve(parent, target), so a rooted relative symbolic link such as \dir names a directory on the link's own drive. The volume-mount walk must not join it under the link's parent.
func TestCanonicalizePathResolvesRootedLinkTargetsBeyondVolumeMounts(t *testing.T) {
	root := t.TempDir()
	drive := filepath.VolumeName(root) + `\`
	driveName, err := windows.UTF16PtrFromString(drive)
	if err != nil {
		t.Fatal(err)
	}
	var volume [windows.MAX_PATH + 1]uint16
	if err := windows.GetVolumeNameForVolumeMountPoint(driveName, &volume[0], uint32(len(volume))); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "marker"), []byte("rooted"), 0o600); err != nil {
		t.Fatal(err)
	}
	rootedTarget := strings.TrimPrefix(target, filepath.VolumeName(target))
	rooted := filepath.Join(root, "rooted")
	// A rooted relative symbolic link needs symlink privilege; a junction always stores an absolute target.
	testenv.Symlink(t, rootedTarget, rooted)
	mount := filepath.Join(root, "mount")
	testenv.RequireDirectoryLink(t, windows.UTF16ToString(volume[:]), mount)
	t.Cleanup(func() {
		if err := os.Remove(mount); err != nil {
			t.Error(err)
		}
	})
	rel, err := filepath.Rel(drive, rooted)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(mount, rel, "marker")
	if data, err := os.ReadFile(path); err != nil || string(data) != "rooted" {
		t.Fatalf("rooted link fixture content=%q error=%v", data, err)
	}
	out, err := exec.CommandContext(t.Context(), "node", "-e", `const fs=require('node:fs'); try{console.log(fs.realpathSync(process.argv[1]))}catch{console.log(process.argv[1])}`, path).Output()
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimRight(string(out), "\r\n")
	if got := CanonicalizePath(path); got != want {
		t.Fatalf("CanonicalizePath(%q)=%q; Node=%q", path, got, want)
	}
}
