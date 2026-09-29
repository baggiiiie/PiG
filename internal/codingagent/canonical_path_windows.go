//go:build windows

package codingagent

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// evalCanonicalPath keeps volume mount points in the path, as Node's realpathSync does. winsymlink=0 makes Go follow both drive junctions and volume mounts, but libuv treats only drive-target mount-point reparse records as symlinks. Ordinary paths retain the standard library's resolution.
// Ports packages/coding-agent/src/utils/paths.ts:canonicalizePath.
func evalCanonicalPath(path string) (string, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err == nil && !isVolumeGUIDPath(canonical) {
		return canonical, nil
	}
	resolved, mount, resolveErr := resolveVolumeMountPath(path)
	if mount {
		return resolved, resolveErr
	}
	return canonical, err
}

func isVolumeGUIDPath(path string) bool {
	return strings.HasPrefix(strings.ToLower(path), `\\?\volume{`)
}

// resolveVolumeMountPath walks an absolute path with Node's volume-mount rule. Stat before reading a link rejects dangling and cyclic links just as Node's realpathSync does. A mount remains in the path, but links before or after it still resolve.
func resolveVolumeMountPath(path string) (resolved string, mount bool, err error) {
	for {
		root := filepath.VolumeName(path) + `\`
		if _, err := os.Stat(root); err != nil {
			return "", mount, err
		}
		resolved = root
		restart := false
		parts := strings.Split(strings.TrimPrefix(path, root), `\`)
		for i, part := range parts {
			if part == "" {
				continue
			}
			resolved = filepath.Join(resolved, part)
			info, err := os.Lstat(resolved)
			if err != nil {
				return "", mount, err
			}
			if info.Mode()&os.ModeSymlink == 0 {
				continue
			}
			if _, err := os.Stat(resolved); err != nil {
				return "", mount, err
			}
			target, err := os.Readlink(resolved)
			if err != nil {
				return "", mount, err
			}
			if isVolumeGUIDPath(target) {
				volumeMount, err := isMountPoint(resolved)
				if err != nil {
					return "", mount, err
				}
				if volumeMount {
					mount = true
					continue
				}
			}
			// Node resolves the target with path.resolve(parent, target): a rooted target such as \dir names the link's own volume, not a child of its parent.
			if !filepath.IsAbs(target) {
				if strings.HasPrefix(target, `\`) {
					target = filepath.VolumeName(resolved) + target
				} else {
					target = filepath.Join(filepath.Dir(resolved), target)
				}
			}
			path = filepath.Join(target, filepath.Join(parts[i+1:]...))
			restart = true
			break
		}
		if !restart {
			return resolved, mount, nil
		}
	}
}

// A symbolic link can also name a volume GUID. Only a mount-point reparse record has Node's directory semantics.
func isMountPoint(path string) (bool, error) {
	// FindFirstFile needs an extended-length path even though os.Lstat and os.Readlink already handle long paths themselves.
	if !strings.HasPrefix(path, `\\?\`) {
		if unc, ok := strings.CutPrefix(path, `\\`); ok {
			path = `\\?\UNC\` + unc
		} else {
			path = `\\?\` + path
		}
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	var data windows.Win32finddata
	handle, err := windows.FindFirstFile(name, &data)
	if err != nil {
		return false, err
	}
	err = windows.FindClose(handle)
	return data.Reserved0 == windows.IO_REPARSE_TAG_MOUNT_POINT, err
}
