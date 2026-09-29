package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"weak"

	"github.com/MichaelKinsy/PiG/coding/extension/installresolver"
	extsource "github.com/MichaelKinsy/PiG/coding/extension/source"
	"github.com/MichaelKinsy/PiG/coding/packagecontent"
	sourceref "github.com/MichaelKinsy/PiG/coding/source"
	"github.com/MichaelKinsy/PiG/internal/codingagent"
	"github.com/MichaelKinsy/PiG/internal/crossspawn"
	"github.com/MichaelKinsy/PiG/internal/resolvepath"
)

type packageCommand string

const (
	packageInstall packageCommand = "install"
	packageRemove  packageCommand = "remove"
	packageUpdate  packageCommand = "update"
	packageList    packageCommand = "list"
)

type packageCLIOptions struct {
	command              packageCommand
	source               string
	sources              []string
	local                bool
	projectTrustOverride *bool
	allPackages          bool // pig update --all: self-update plus every configured package
	extensionsOnly       bool // pig update --extensions: update every package, not pig itself
	selfOnly             bool
	modelsOnly           bool
	extensionSource      string
	force                bool
	validateOnly         bool
	jsonOutput           bool
	help                 bool
	invalidOption        string
	missingValue         string
	invalidArgument      string
	conflict             string
	// showExtensionsSkippedNote marks a bare `pig update`, which updates pig only.
	showExtensionsSkippedNote bool
}

type configuredPackage struct {
	Source        codingagent.PackageSource
	Scope         string
	InstalledPath string
	// ResolvedSource owns the inherited installation when a project delta retains a different metadata source.
	ResolvedSource string
}

// isSelfUpdateTarget reports whether an update target names pig itself.
// Mirrors upstream `source === "self" || source === "<app-name>"`
// (package-manager-cli.ts): pig accepts "self" and the app name "pig". These
// are synonyms, not backward-compat aliases; bare `pig update` means the same.
func isSelfUpdateTarget(source string) bool {
	switch strings.TrimSpace(strings.ToLower(source)) {
	case "self", codingagent.AppName:
		return true
	default:
		return false
	}
}

func parsePackageCommand(args []string) (*packageCLIOptions, bool) {
	if len(args) == 0 {
		return nil, false
	}
	cmd := args[0]
	if cmd == "uninstall" {
		cmd = "remove"
	}
	if cmd == "config" {
		return nil, false
	}
	if cmd != "install" && cmd != "remove" && cmd != "update" && cmd != "list" {
		return nil, false
	}
	opts := &packageCLIOptions{command: packageCommand(cmd)}
	var positionals []string
	for i := 1; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help":
			opts.help = true
		case arg == "-l" || arg == "--local":
			if opts.command == packageInstall || opts.command == packageRemove {
				opts.local = true
			} else if opts.invalidOption == "" {
				opts.invalidOption = arg
			}
		case arg == "--approve" || arg == "-a":
			opts.projectTrustOverride = new(true)
		case arg == "--no-approve" || arg == "-na":
			opts.projectTrustOverride = new(false)
		case arg == "--validate-only" || arg == "--check":
			// pig additive (D28): validate-only install mode; see
			// docs/additive-features.md.
			if opts.command == packageInstall {
				opts.validateOnly = true
			} else if opts.invalidOption == "" {
				opts.invalidOption = arg
			}
		case arg == "--json":
			if opts.command == packageInstall {
				opts.jsonOutput = true
			} else if opts.invalidOption == "" {
				opts.invalidOption = arg
			}
		case arg == "--all":
			if opts.command == packageUpdate {
				opts.allPackages = true
			} else if opts.invalidOption == "" {
				opts.invalidOption = arg
			}
		case arg == "--extensions":
			if opts.command == packageUpdate {
				opts.extensionsOnly = true
			} else if opts.invalidOption == "" {
				opts.invalidOption = arg
			}
		case arg == "--models":
			if opts.command == packageUpdate {
				opts.modelsOnly = true
			} else if opts.invalidOption == "" {
				opts.invalidOption = arg
			}
		case arg == "--self":
			if opts.command == packageUpdate {
				opts.selfOnly = true
			} else if opts.invalidOption == "" {
				opts.invalidOption = arg
			}
		case arg == "--force":
			if opts.command == packageUpdate {
				opts.force = true
			} else if opts.invalidOption == "" {
				opts.invalidOption = arg
			}
		case arg == "--extension":
			if opts.command != packageUpdate {
				if opts.invalidOption == "" {
					opts.invalidOption = arg
				}
				continue
			}
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				if opts.missingValue == "" {
					opts.missingValue = arg
				}
				continue
			}
			i++
			if opts.extensionSource != "" {
				opts.conflict = "--extension can only be provided once"
				continue
			}
			opts.extensionSource = args[i]
		case arg == "--set":
			if opts.command != packageInstall {
				if opts.invalidOption == "" {
					opts.invalidOption = arg
				}
				continue
			}
			if i+1 >= len(args) {
				if opts.invalidOption == "" {
					opts.invalidOption = arg
				}
				continue
			}
			i++
			opts.sources = append(opts.sources, splitInstallSetSources(args[i])...)
		case strings.HasPrefix(arg, "--set="):
			if opts.command == packageInstall {
				opts.sources = append(opts.sources, splitInstallSetSources(strings.TrimPrefix(arg, "--set="))...)
			} else if opts.invalidOption == "" {
				opts.invalidOption = "--set"
			}
		default:
			if strings.HasPrefix(arg, "-") {
				if opts.invalidOption == "" {
					opts.invalidOption = arg
				}
				continue
			}
			opts.sources = append(opts.sources, arg)
			if arg != "" {
				positionals = append(positionals, arg)
			}
		}
	}
	opts.sources = compactStrings(opts.sources)
	if len(opts.sources) > 0 {
		opts.source = opts.sources[0]
	}
	// The first positional is the source and the next one is unexpected.
	// pig additive (D28): --validate-only installs accept several sources.
	if len(positionals) > 1 && (opts.command != packageInstall || !opts.validateOnly) {
		opts.invalidArgument = positionals[1]
	}
	if opts.command == packageUpdate {
		// The first conflict found wins: --all, then --models, --extension, and a positional target.
		conflict := func(message string) {
			if opts.conflict == "" {
				opts.conflict = message
			}
		}
		if opts.allPackages && (opts.selfOnly || opts.extensionsOnly || opts.modelsOnly || opts.extensionSource != "") {
			conflict("--all cannot be combined with --self, --extensions, --models, or --extension")
		}
		if opts.allPackages && opts.source != "" {
			conflict("--all cannot be combined with a positional source")
		}
		switch {
		case opts.modelsOnly:
			if opts.selfOnly || opts.extensionsOnly || opts.allPackages || opts.extensionSource != "" {
				conflict("--models cannot be combined with --self, --extensions, --all, or --extension")
			}
			if opts.source != "" {
				conflict("--models cannot be combined with a positional source")
			}
		case opts.extensionSource != "":
			if opts.selfOnly || opts.extensionsOnly || opts.allPackages {
				conflict("--extension cannot be combined with --self, --extensions, or --all")
			}
			if opts.source != "" {
				conflict("--extension cannot be combined with a positional source")
			}
			if opts.conflict == "" {
				opts.source = opts.extensionSource
			}
		case opts.source != "":
			if isSelfUpdateTarget(opts.source) {
				if opts.extensionsOnly {
					opts.source = ""
					opts.allPackages = true
					opts.extensionsOnly = false
				}
			} else if opts.extensionsOnly || opts.selfOnly || opts.allPackages {
				conflict("positional update targets cannot be combined with --self, --extensions, or --all")
			}
		case opts.allPackages:
		case opts.selfOnly && opts.extensionsOnly:
			opts.allPackages = true
			opts.selfOnly = false
			opts.extensionsOnly = false
		case !opts.selfOnly && !opts.extensionsOnly:
			opts.showExtensionsSkippedNote = true
		}
	}
	return opts, true
}

func packageUsage(cmd packageCommand) string {
	switch cmd {
	case packageInstall:
		return "pig install <source> [-l] [--approve|--no-approve]"
	case packageRemove:
		return "pig remove <source> [-l] [--approve|--no-approve]"
	case packageUpdate:
		return "pig update [source|self|pig] [--self|--extensions|--models|--all] [--extension <source>] [--approve|--no-approve] [--force]"
	case packageList:
		return "pig list [--approve|--no-approve]"
	default:
		return "pig <package-command>"
	}
}

func printPackageCommandHelp(cmd packageCommand) {
	switch cmd {
	case packageInstall:
		fmt.Print("Usage:\n  pig install <source> [-l] [--approve|--no-approve]\n  pig install --validate-only [--json] <source>...\n  pig install --validate-only [--json] --set <source[,source...]>\n\nInstall a package and add it to settings. With --validate-only, validate/build/start one or more packages without installing them.\n\nOptions:\n  -l, --local        Install project-locally (.pig/settings.json)\n  -a, --approve     Trust project-local files for this command\n  -na, --no-approve Ignore project-local files for this command\n  --validate-only    Validate/build/start package refs without installing them\n  --json             Emit JSON validation output with --validate-only\n  --set              Validate a comma- or whitespace-separated extension set\n\nExamples:\n  pig install npm:@foo/bar\n  pig install git:github.com/user/repo\n  pig install git:git@github.com:user/repo\n  pig install https://github.com/user/repo\n  pig install ssh://git@github.com/user/repo\n  pig install ./local/path\n  pig install ./local/path --validate-only --json\n  pig install --validate-only --json ./ext-a ./ext-b\n")
	case packageRemove:
		fmt.Print("Usage:\n  pig remove <source> [-l] [--approve|--no-approve]\n\nRemove a package and its source from settings.\nAlias: pig uninstall <source> [-l]\n\nOptions:\n  -l, --local       Remove from project settings (.pig/settings.json)\n  -a, --approve     Trust project-local files for this command\n  -na, --no-approve Ignore project-local files for this command\n\nExamples:\n  pig remove npm:@foo/bar\n  pig uninstall npm:@foo/bar\n\n")
	case packageUpdate:
		fmt.Print("Usage:\n  " + packageUsage(packageUpdate) + "\n\nUpdate pig, installed packages, or model catalogs.\n\nOptions:\n  --self                  Update pig only (default when no target is given)\n  --extensions            Update installed packages only\n  --models                Refresh model catalogs only\n  --all                   Update pig and installed packages\n  --extension <source>    Update one package only\n  -a, --approve           Trust project-local files for this command\n  -na, --no-approve       Ignore project-local files for this command\n  --force                 Reinstall pig even if the current version is latest\n\nShort forms:\n  pig update                Update pig only\n  pig update --all          Update pig and all extensions\n  pig update --models       Refresh model catalogs only\n  pig update <source>       Update one package\n  pig update pig            Update pig only (self works as alias to pig)\n\n")
	case packageList:
		fmt.Print("Usage:\n  pig list [--approve|--no-approve]\n\nList installed packages from user and project settings.\n\nOptions:\n  -a, --approve     Trust project-local files for this command\n  -na, --no-approve Ignore project-local files for this command\n")
	}
}

func packageContext() (cwd, agentDir string, sm *codingagent.SettingsManager, err error) {
	cwd, err = os.Getwd()
	if err != nil {
		return "", "", nil, err
	}
	agentDir = codingagent.AgentDir()
	// pig additive (D40): pre-session inspection resolves saved/default trust without invoking extension handlers.
	sm = codingagent.NewSettingsManagerWithProjectTrust(cwd, agentDir, false)
	trusted, err := resolveProjectTrusted(context.Background(), projectTrustResolutionOptions{
		CWD: cwd, Store: codingagent.NewProjectTrustStore(agentDir), Default: sm.GetGlobalSettings().DefaultProjectTrust,
	})
	if err != nil {
		return "", "", nil, err
	}
	sm.SetProjectTrusted(trusted)
	return cwd, agentDir, sm, nil
}

func reportSettingsErrors(sm *codingagent.SettingsManager, context string) {
	for _, serr := range sm.DrainErrors() {
		fmt.Fprintf(os.Stderr, "Warning (%s, %s settings): %v\n", context, serr.Scope, serr.Error)
	}
}

func init() {
	// pig additive (D18): Piglets materialize Package/direct sources through
	// the core installer without mutating Package settings.
	installresolver.SetMaterializer(func(cwd, source, scope string, stdout, _ io.Writer) (string, error) {
		local := scope == "project"
		if scope != "user" && scope != "project" {
			return "", fmt.Errorf("unknown package materialization scope %q", scope)
		}
		sm, err := createPackageCommandSettings(context.Background(), cwd, codingagent.AgentDir(), &packageCLIOptions{command: packageInstall})
		if err != nil {
			return "", err
		}
		if local && !sm.IsProjectTrusted() {
			return "", errors.New("Project is not trusted; refusing to access project package storage")
		}
		source = strings.TrimSpace(source)
		kind := detectSourceKind(source)
		switch kind {
		case "local":
			root, err := resolveInputPackageSourceRoot(cwd, sm, source)
			if err != nil {
				return "", err
			}
			if _, err := os.Stat(root); err != nil {
				return "", fmt.Errorf("Path does not exist: %s", root)
			}
			return root, nil
		case "npm":
			_, _ = fmt.Fprintf(stdout, "[%s] fetching %s\n", scope, source)
			if err := installManagedNPM(cwd, sm, source, local); err != nil {
				return "", err
			}
		case "git":
			_, _ = fmt.Fprintf(stdout, "[%s] fetching %s\n", scope, source)
			if err := installManagedGit(cwd, sm, source, local); err != nil {
				return "", err
			}
		default:
			return "", fmt.Errorf("unsupported package source: %s", source)
		}
		return sourceRootForResources(cwd, sm, source, local)
	})
	installresolver.SetInstaller(func(cwd, source, scope string, stdout, stderr io.Writer) error {
		sm, err := createPackageCommandSettings(context.Background(), cwd, codingagent.AgentDir(), &packageCLIOptions{command: packageInstall})
		if err != nil {
			return err
		}
		local := scope == "project"
		return installAndPersistPackage(cwd, sm, source, local, packageProgressPrinter(stdout))
	})
}

func runPackageCommand(args []string, runtimeOptions ...packageCommandRuntimeOptions) int {
	if len(args) > 0 && args[0] == "package" {
		return runPackageManagementCommand(args[1:])
	}
	opts, ok := parsePackageCommand(args)
	if !ok {
		return -1
	}
	if opts.help {
		printPackageCommandHelp(opts.command)
		return 0
	}
	if opts.invalidOption != "" {
		fmt.Fprintf(os.Stderr, "Unknown option %s for %q.\n", opts.invalidOption, opts.command)
		fmt.Fprintf(os.Stderr, "Use %q or %q.\n", "pig --help", packageUsage(opts.command))
		return 1
	}
	if opts.missingValue != "" {
		fmt.Fprintf(os.Stderr, "Missing value for %s.\n", opts.missingValue)
		fmt.Fprintf(os.Stderr, "Usage: %s\n", packageUsage(opts.command))
		return 1
	}
	if opts.invalidArgument != "" {
		fmt.Fprintf(os.Stderr, "Unexpected argument %s.\n", opts.invalidArgument)
		fmt.Fprintf(os.Stderr, "Usage: %s\n", packageUsage(opts.command))
		return 1
	}
	if opts.conflict != "" {
		fmt.Fprintln(os.Stderr, opts.conflict)
		fmt.Fprintf(os.Stderr, "Usage: %s\n", packageUsage(opts.command))
		return 1
	}
	if (opts.command == packageInstall || opts.command == packageRemove) && opts.source == "" {
		fmt.Fprintf(os.Stderr, "Missing %s source.\n", opts.command)
		fmt.Fprintf(os.Stderr, "Usage: %s\n", packageUsage(opts.command))
		return 1
	}
	if opts.command == packageInstall && len(opts.sources) > 1 && !opts.validateOnly {
		fmt.Fprintln(os.Stderr, "Multiple install sources require --validate-only.")
		fmt.Fprintf(os.Stderr, "Usage: %s\n", packageUsage(opts.command))
		return 1
	}
	if opts.command == packageUpdate && opts.modelsOnly {
		if err := refreshModelCatalogs(codingagent.AgentDir()); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		return 0
	}

	cwd, err := os.Getwd()
	if err != nil {
		printCLIError("%v", err)
		return 1
	}
	sm, err := createPackageCommandSettings(context.Background(), cwd, codingagent.AgentDir(), opts, runtimeOptions...)
	if err != nil {
		printCLIError("%v", err)
		return 1
	}
	if opts.local && !sm.IsProjectTrusted() && (opts.command == packageInstall || opts.command == packageRemove) {
		fmt.Fprintln(os.Stderr, "Project is not trusted. Use --approve to modify local package config.")
		return 1
	}
	reportSettingsErrors(sm, "package command")

	if opts.command == packageUpdate {
		// pig divergence (D39): route bare update to the proven binary owner.
		return runUpdateCommand(cwd, sm, opts)
	}

	switch opts.command {
	case packageInstall:
		installSources := append([]string(nil), opts.sources...)
		if opts.validateOnly {
			// pig additive (D28): emit the validation report instead of installing.
			return validateInstallSources(cwd, sm, installSources, opts.jsonOutput)
		}
		installSource := installSources[0]
		if err := installAndPersistPackage(cwd, sm, installSource, opts.local, packageProgressPrinter(os.Stdout)); err != nil {
			printCLIError("%v", err)
			return 1
		}
		fmt.Printf("Installed %s\n", opts.source)
		return 0
	case packageRemove:
		fmt.Printf("Removing %s...\n", opts.source)
		removed, err := removeAndPersistPackage(cwd, sm, opts.source, opts.local)
		if err != nil {
			printCLIError("%v", err)
			return 1
		}
		if !removed {
			fmt.Fprintf(os.Stderr, "No matching package found for %s\n", opts.source)
			return 1
		}
		fmt.Printf("Removed %s\n", opts.source)
		return 0
	case packageList:
		return listPackages(cwd, sm)
	default:
		return 1
	}
}

// runUpdateCommand routes `pig update`. Bare or a self target self-updates the
// binary (D39); `<source>` updates one package; `--all` refreshes packages and
// then self-updates, matching upstream's observable operation order.
func runUpdateCommand(cwd string, sm *codingagent.SettingsManager, opts *packageCLIOptions) int {
	if opts.showExtensionsSkippedNote {
		fmt.Printf("Extensions are skipped. Run %s update --extensions to update extensions.\n", codingagent.AppName)
	}
	if opts.source != "" && !isSelfUpdateTarget(opts.source) {
		if err := updatePackages(cwd, sm, opts.source, packageProgressPrinter(os.Stdout)); err != nil {
			printCLIError("%v", err)
			return 1
		}
		fmt.Printf("Updated %s\n", opts.source)
		return 0
	}

	// --extensions updates every package without touching pig itself.
	if opts.extensionsOnly && !opts.allPackages {
		if err := updatePackages(cwd, sm, "", packageProgressPrinter(os.Stdout)); err != nil {
			printCLIError("%v", err)
			return 1
		}
		fmt.Println("Updated packages")
		return 0
	}

	if !opts.allPackages {
		return runSelfUpdate(opts.force)
	}
	if err := updatePackages(cwd, sm, "", packageProgressPrinter(os.Stdout)); err != nil {
		printCLIError("%v", err)
		return 1
	}
	fmt.Println("Updated packages")
	return runSelfUpdate(opts.force)
}

func installAndPersistPackage(cwd string, sm *codingagent.SettingsManager, source string, local bool, progress ProgressCallback) error {
	pkg := codingagent.PackageSource{Source: source}
	if err := installPackageArtifacts(cwd, sm, pkg, local, progress); err != nil {
		return err
	}
	if err := verifyPackageContributesResources(cwd, sm, source, local); err != nil {
		return err
	}
	if _, err := addSourceToSettings(cwd, sm, source, local); err != nil {
		return err
	}
	return nil
}

// pig divergence (D57): reject a proven extension root that Package discovery
// cannot load.
func verifyPackageContributesResources(cwd string, sm *codingagent.SettingsManager, source string, local bool) error {
	root := installedPathForSource(cwd, sm, source, local)
	if root == "" {
		// The source resolved to no local root (a remote form this build cannot
		// inspect). Nothing to assert against, so stay out of the way.
		return nil
	}
	resources, err := packagecontent.Discover(root)
	if err != nil {
		return fmt.Errorf("inspect installed package %s: %w", source, err)
	}
	if packageResourceCount(resources) > 0 {
		return nil
	}
	// Refuse only a complete extension contract. Empty Packages remain valid.
	if !directoryIsProvablyAnExtension(root) {
		return nil
	}
	return fmt.Errorf("%s is an extension, not a package, so installing it as one loads nothing.\n"+
		"Load it directly with `pig -e %s`, put it in ~/.pig/agent/extensions/, or publish it inside a "+
		"package's extensions/ directory", source, source)
}

// directoryIsProvablyAnExtension accepts a statically proven factory or standalone contract. Node factory resolution only selects an entrypoint; proving its exports requires execution, which Package installation must not perform.
func directoryIsProvablyAnExtension(root string) bool {
	definition, err := extsource.Resolve(root)
	return err == nil && (definition.Language != "node" || definition.Form != extsource.Factory)
}

func packageResourceCount(r packagecontent.Resources) int {
	return len(r.ExtensionEntries) + len(r.SkillDirs) + len(r.PromptFiles) + len(r.ThemeFiles) +
		len(r.AgentFiles) + len(r.MCPFiles) + len(r.HookFiles) + len(r.AgentEnvironments)
}

func getGitDependencyInstallArgs(sm *codingagent.SettingsManager) []string {
	configuredCommand := sm.GetNpmCommand()
	if len(configuredCommand) > 0 {
		return []string{"install"}
	}
	return []string{"install", "--omit=dev"}
}

// removeAndPersistPackage runs the removal with the caller's settings before persisting, so failures retain both the package record and command overrides.
func removeAndPersistPackage(cwd string, sm *codingagent.SettingsManager, source string, local bool) (bool, error) {
	if err := removePackageArtifacts(cwd, sm, source, local); err != nil {
		return false, err
	}
	return removeSourceFromSettings(cwd, sm, source, local)
}

func updatePackages(cwd string, sm *codingagent.SettingsManager, source string, progress ProgressCallback) error {
	// update reads configured sources, not installation paths; pinned entries must not run legacy-root lookups.
	pkgs := configuredPackageSources(sm)
	if source == "" {
		return updateConfiguredSources(cwd, sm, pkgs, progress)
	}
	identity := packageIdentityFromInput(cwd, source)
	var matched []configuredPackage
	for _, pkg := range pkgs {
		if packageIdentityFromStored(cwd, pkg.Source.Source, pkg.Scope == "project") == identity {
			matched = append(matched, pkg)
		}
	}
	if len(matched) == 0 {
		return errors.New(noMatchingPackageMessage(cwd, source, pkgs))
	}
	return updateConfiguredSources(cwd, sm, matched, progress)
}

type gitPackageSource struct {
	host   string
	path   string
	ref    string
	pinned bool
}

func settingsBaseDirForScope(cwd string, local bool) string {
	if local {
		return codingagent.ProjectConfigDir(cwd)
	}
	return codingagent.AgentDir()
}

func settingsBaseDirForManager(sm *codingagent.SettingsManager, local bool) string {
	if local {
		return codingagent.ProjectConfigDir(sm.CWD())
	}
	return sm.AgentDir()
}

// normalizePackageSourceForSettings stores local package paths relative to the
// settings directory, matching upstream.
func normalizePackageSourceForSettings(baseDir, cwd, source string) string {
	kind := detectSourceKind(source)
	if kind == "npm" && !strings.HasPrefix(strings.TrimSpace(source), "npm:") {
		return "npm:" + strings.TrimSpace(source)
	}
	if kind != "local" {
		return source
	}
	resolved := resolveInputLocalPackageRoot(cwd, source)
	rel, err := filepath.Rel(baseDir, resolved)
	if err != nil {
		return source
	}
	return filepath.Clean(rel)
}

// addSourceToSettings reports whether it adds a source or replaces its ref, retaining filters and avoiding writes for identical sources.
func addSourceToSettings(cwd string, sm *codingagent.SettingsManager, source string, local bool) (bool, error) {
	baseDir := settingsBaseDirForManager(sm, local)
	normalized := normalizePackageSourceForSettings(baseDir, cwd, source)
	current := sm.GetGlobalSettings().Packages
	if local {
		current = sm.GetProjectSettings().Packages
	}
	current = slices.Clone(current)
	index := slices.IndexFunc(current, func(existing codingagent.PackageSource) bool {
		return packageMatchKeyForStoredBase(baseDir, existing.Source) == packageMatchKeyForInput(cwd, source)
	})
	if index >= 0 {
		if current[index].Source == normalized {
			return false, nil
		}
		current[index].Source = normalized
	} else {
		current = append(current, codingagent.PackageSource{Source: normalized})
	}
	if local {
		return true, sm.SetProjectPackages(current)
	}
	return true, sm.SetPackages(current)
}

func removeSourceFromSettings(cwd string, sm *codingagent.SettingsManager, source string, local bool) (bool, error) {
	if local {
		current := sm.GetProjectSettings().Packages
		next := make([]codingagent.PackageSource, 0, len(current))
		removed := false
		baseDir := settingsBaseDirForManager(sm, true)
		for _, pkg := range current {
			if packageMatchKeyForStoredBase(baseDir, pkg.Source) == packageMatchKeyForInput(cwd, source) {
				removed = true
				continue
			}
			next = append(next, pkg)
		}
		if !removed {
			return false, nil
		}
		return true, sm.SetProjectPackages(next)
	}
	current := sm.GetGlobalSettings().Packages
	next := make([]codingagent.PackageSource, 0, len(current))
	removed := false
	baseDir := settingsBaseDirForManager(sm, false)
	for _, pkg := range current {
		if packageMatchKeyForStoredBase(baseDir, pkg.Source) == packageMatchKeyForInput(cwd, source) {
			removed = true
			continue
		}
		next = append(next, pkg)
	}
	if !removed {
		return false, nil
	}
	return true, sm.SetPackages(next)
}

func listPackages(cwd string, sm *codingagent.SettingsManager) int {
	pkgs := listConfiguredPackages(cwd, sm)
	if len(pkgs) == 0 {
		fmt.Println("No packages installed.")
		return 0
	}
	userPkgs := make([]configuredPackage, 0)
	projectPkgs := make([]configuredPackage, 0)
	for _, pkg := range pkgs {
		if pkg.Scope == "project" {
			projectPkgs = append(projectPkgs, pkg)
		} else {
			userPkgs = append(userPkgs, pkg)
		}
	}
	format := func(title string, pkgs []configuredPackage) {
		if len(pkgs) == 0 {
			return
		}
		fmt.Println(title)
		for _, pkg := range pkgs {
			display := pkg.Source.Source
			if pkg.Source.Filtered() {
				display += " (filtered)"
			}
			fmt.Printf("  %s\n", display)
			if pkg.InstalledPath != "" {
				fmt.Printf("    %s\n", pkg.InstalledPath)
			}
		}
	}
	format("User packages:", userPkgs)
	if len(userPkgs) > 0 && len(projectPkgs) > 0 {
		fmt.Println()
	}
	format("Project packages:", projectPkgs)
	return 0
}

func listConfiguredPackages(cwd string, sm *codingagent.SettingsManager) []configuredPackage {
	packages := configuredPackageSources(sm)
	for i := range packages {
		pkg := &packages[i]
		pkg.InstalledPath = installedPathForConfiguredSource(cwd, sm, pkg.Source.Source, pkg.Scope == "project")
	}
	return packages
}

func configuredPackageSources(sm *codingagent.SettingsManager) []configuredPackage {
	global := sm.GetGlobalSettings().Packages
	project := sm.GetProjectSettings().Packages
	packages := make([]configuredPackage, 0, len(global)+len(project))
	for _, pkg := range global {
		packages = append(packages, configuredPackage{Source: pkg, Scope: "user"})
	}
	for _, pkg := range project {
		packages = append(packages, configuredPackage{Source: pkg, Scope: "project"})
	}
	return packages
}

func installPackageArtifacts(cwd string, sm *codingagent.SettingsManager, pkg codingagent.PackageSource, local bool, progress ProgressCallback) error {
	if local && !sm.IsProjectTrusted() {
		return errors.New("Project is not trusted; refusing to access project package storage")
	}
	return withProgress(progress, "install", pkg.Source, fmt.Sprintf("Installing %s...", pkg.Source), func() error {
		switch detectSourceKind(pkg.Source) {
		case "local":
			root, err := resolveInputPackageSourceRoot(cwd, sm, pkg.Source)
			if err != nil {
				return err
			}
			if _, err := os.Stat(root); err != nil {
				return fmt.Errorf("Path does not exist: %s", root)
			}
			return nil
		case "npm":
			return installManagedNPM(cwd, sm, pkg.Source, local)
		case "git":
			return installManagedGit(cwd, sm, pkg.Source, local)
		default:
			return fmt.Errorf("unsupported package source: %s", pkg.Source)
		}
	})
}

func removePackageArtifacts(cwd string, sm *codingagent.SettingsManager, source string, local bool) error {
	if local && !sm.IsProjectTrusted() {
		return errors.New("Project is not trusted; refusing to access project package storage")
	}
	switch detectSourceKind(source) {
	case "npm":
		return uninstallManagedNPM(cwd, sm, source, local)
	case "git":
		checkout, err := gitCheckoutPath(cwd, source, local)
		if err != nil {
			return err
		}
		if configuredGitCheckoutInUse(cwd, sm, source, local) {
			return nil
		}
		if err := os.RemoveAll(checkout); err != nil {
			return err
		}
		if err := removeGitUpdateMarker(gitUpdateMarkerPath(checkout)); err != nil {
			return err
		}
		return pruneEmptyGitParents(checkout, codingagent.GitInstallRoot(cwd, sm.AgentDir(), local))
	default:
		return nil
	}
}

func configuredGitCheckoutInUse(cwd string, sm *codingagent.SettingsManager, removedSource string, local bool) bool {
	removed, err := sourceref.Parse(removedSource, sourceref.Options{Bare: sourceref.BareReject})
	if err != nil || removed.Kind != sourceref.KindGit {
		return false
	}
	wantScope := "user"
	if local {
		wantScope = "project"
	}
	for _, pkg := range listConfiguredPackages(cwd, sm) {
		if pkg.Scope != wantScope || packageMatchKeyForStoredBase(settingsBaseDirForManager(sm, local), pkg.Source.Source) == packageMatchKeyForInput(cwd, removedSource) {
			continue
		}
		ref, err := sourceref.Parse(pkg.Source.Source, sourceref.Options{Bare: sourceref.BareReject})
		if err == nil && ref.Kind == sourceref.KindGit && ref.GitHost == removed.GitHost && ref.GitPath == removed.GitPath {
			return true
		}
	}
	return false
}

func sourceRootForResources(cwd string, sm *codingagent.SettingsManager, source string, local bool) (string, error) {
	switch detectSourceKind(source) {
	case "npm":
		ref, err := parseNpmInstallRef(source)
		if err != nil {
			return "", err
		}
		return npmInstallPath(cwd, sm, ref, local), nil
	case "git":
		return gitInstallPath(cwd, source, local)
	case "local":
		baseDir := settingsBaseDirForScope(cwd, local)
		return resolveLocalPackageRoot(baseDir, source)
	default:
		return "", fmt.Errorf("unsupported package source: %s", source)
	}
}

func resolveInputPackageSourceRoot(cwd string, sm *codingagent.SettingsManager, source string) (string, error) {
	if detectSourceKind(source) != "local" {
		return sourceRootForResources(cwd, sm, source, false)
	}
	return resolveLocalPackageRoot(cwd, source)
}

func resolveInputLocalPackageRoot(cwd, source string) string {
	root, _ := resolveLocalPackageRoot(cwd, source)
	return root
}

func resolveLocalPackageRoot(baseDir, source string) (string, error) {
	return resolvepath.ResolveTrimmed(source, baseDir)
}

func npmInstallPath(cwd string, sm *codingagent.SettingsManager, ref sourceref.Ref, local bool) string {
	installRoot := npmInstallRoot(cwd, sm, ref, local)
	managedPath := filepath.Join(installRoot, "node_modules", filepath.FromSlash(ref.NPMName))
	if local || ref.NPMRegistry != "" {
		return managedPath
	}
	if _, err := os.Stat(managedPath); err == nil {
		return managedPath
	}
	if legacyPath := getLegacyGlobalNpmInstallPath(sm, ref.NPMName); legacyPath != "" {
		if _, err := os.Stat(legacyPath); err == nil {
			return legacyPath
		}
	}
	return managedPath
}

func npmInstallRoot(cwd string, sm *codingagent.SettingsManager, ref sourceref.Ref, local bool) string {
	root := codingagent.NPMInstallRoot(cwd, sm.AgentDir(), local)
	if ref.NPMRegistry == "" {
		return root
	}
	digest := sha256.Sum256([]byte(ref.NPMRegistry))
	return filepath.Join(root, "registries", fmt.Sprintf("%x", digest[:8]))
}

// getLegacyGlobalNpmInstallPath follows Pi's pnpm lookup, then its global-root fallback.
func getLegacyGlobalNpmInstallPath(sm *codingagent.SettingsManager, packageName string) string {
	if resolved, err := getPnpmGlobalPackagePath(sm, packageName); err != nil {
		return ""
	} else if resolved != "" {
		return resolved
	}
	root, err := getGlobalNpmRoot(sm)
	if err != nil || root == "" {
		return ""
	}
	return filepath.Join(root, filepath.FromSlash(packageName))
}

// globalNpmRoot is Pi's DefaultPackageManager globalNpmRoot and globalNpmRootCommandKey. Pi keeps that cache on each DefaultPackageManager, and creates fresh managers (with empty caches) around the same SettingsManager, for example for every interactive package-update check (interactive-mode.ts:1202-1206). Pig threads the SettingsManager itself through package resolution, so each SettingsManager owns one entry until it is collected; a later lookup with an unchanged command reuses the root where Pi would run it again. The mutex also holds concurrent lookups while the first runs the command, as Pi's synchronous spawn does.
type globalNpmRoot struct {
	mu         sync.Mutex
	commandKey string
	root       string
}

var globalNpmRoots struct {
	mu      sync.Mutex
	entries map[weak.Pointer[codingagent.SettingsManager]]*globalNpmRoot
}

func globalNpmRootFor(sm *codingagent.SettingsManager) *globalNpmRoot {
	key := weak.Make(sm)
	globalNpmRoots.mu.Lock()
	defer globalNpmRoots.mu.Unlock()
	if entry := globalNpmRoots.entries[key]; entry != nil {
		return entry
	}
	if globalNpmRoots.entries == nil {
		globalNpmRoots.entries = make(map[weak.Pointer[codingagent.SettingsManager]]*globalNpmRoot)
	}
	entry := &globalNpmRoot{}
	globalNpmRoots.entries[key] = entry
	runtime.AddCleanup(sm, func(key weak.Pointer[codingagent.SettingsManager]) {
		globalNpmRoots.mu.Lock()
		delete(globalNpmRoots.entries, key)
		globalNpmRoots.mu.Unlock()
	}, key)
	return entry
}

// getGlobalNpmRoot is Pi's getGlobalNpmRoot: the selected command's global root, reused until the npmCommand argv changes. An empty root is not reused, and a failed lookup keeps the previous entry.
func getGlobalNpmRoot(sm *codingagent.SettingsManager) (string, error) {
	command := defaultNpmCommand(sm)
	commandKey := strings.Join(command, "\x00")
	entry := globalNpmRootFor(sm)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.root != "" && entry.commandKey == commandKey {
		return entry.root, nil
	}
	var root string
	if npmCommandName(command) == "bun" {
		binDir, err := runCmd(command[0], append(command[1:], "pm", "bin", "-g")...)
		if err != nil || binDir == "" {
			return "", err
		}
		root = filepath.Join(filepath.Dir(binDir), "install", "global", "node_modules")
	} else {
		var err error
		if root, err = runCmd(command[0], append(command[1:], "root", "-g")...); err != nil {
			return "", err
		}
	}
	entry.commandKey, entry.root = commandKey, root
	return root, nil
}

func getPnpmGlobalPackagePath(sm *codingagent.SettingsManager, packageName string) (string, error) {
	npmCommand := defaultNpmCommand(sm)
	if npmCommandName(npmCommand) != "pnpm" {
		return "", nil
	}
	output, err := runCmd(npmCommand[0], append(npmCommand[1:], "list", "-g", "--depth", "0", "--json")...)
	if err != nil {
		return "", err
	}
	var entries []struct {
		Dependencies map[string]struct {
			Path string `json:"path"`
		} `json:"dependencies"`
	}
	if err := json.Unmarshal([]byte(output), &entries); err != nil {
		return "", err
	}
	if entries == nil {
		return "", errors.New("invalid pnpm global package list")
	}
	for _, entry := range entries {
		if dep, ok := entry.Dependencies[packageName]; ok && dep.Path != "" {
			return dep.Path, nil
		}
	}
	return "", nil
}

// defaultNpmCommand uses the caller's resolved trust and overrides; helper calls must not reload project settings.
func defaultNpmCommand(sm *codingagent.SettingsManager) []string {
	if cmd := sm.GetNpmCommand(); len(cmd) > 0 {
		return append([]string(nil), cmd...)
	}
	return []string{"npm"}
}

// npmCommandName uses the executable after the last --, preserves case, and removes only .cmd/.exe suffixes without spawning the command (Pi package-manager.ts:1760-1764).
func npmCommandName(cmd []string) string {
	if len(cmd) == 0 {
		return ""
	}
	idx := -1
	for i, part := range cmd {
		if part == "--" {
			idx = i
		}
	}
	target := cmd[0]
	if idx >= 0 {
		if idx+1 == len(cmd) {
			return ""
		}
		target = cmd[idx+1]
	}
	if target == "" {
		return ""
	}
	base := filepath.Base(target)
	ext := filepath.Ext(base)
	if strings.EqualFold(ext, ".cmd") || strings.EqualFold(ext, ".exe") {
		return strings.TrimSuffix(base, ext)
	}
	return base
}

func installedPathForSource(cwd string, sm *codingagent.SettingsManager, source string, local bool) string {
	path, err := sourceRootForResources(cwd, sm, source, local)
	if err == nil {
		if _, statErr := os.Stat(path); statErr == nil {
			return path
		}
	}
	return ""
}

func packageMatchKeyForStoredBase(baseDir, source string) string {
	return packageSourceIdentity(baseDir, source)
}

func packageMatchKeyForInput(cwd, source string) string {
	return packageMatchKey(cwd, source, func() string { return cwd })
}

func packageMatchKey(cwd, source string, localBaseDir func() string) string {
	baseDir := localBaseDir()
	if baseDir == "" {
		baseDir = cwd
	}
	return packageSourceIdentity(baseDir, source)
}

func packageIdentityFromInput(cwd, source string) string {
	return packageSourceIdentity(cwd, source)
}

func packageIdentityFromStored(cwd, source string, local bool) string {
	return packageSourceIdentity(settingsBaseDirForScope(cwd, local), source)
}

// packageSourceIdentity centralizes upstream package identity and Pig contributed scheme identity. Identity matching follows upstream parseSource: an unprefixed value is local.
//
// pig additive (D18): registered contributed source schemes participate in
// the same deterministic identity contract as upstream npm/git/local sources.
func packageSourceIdentity(baseDir, raw string) string {
	ref, err := sourceref.Parse(raw, sourceref.Options{
		BaseDir:          baseDir,
		Bare:             sourceref.BareLocal,
		AllowContributed: true,
	})
	if err != nil {
		return "unsupported:" + strings.TrimSpace(raw)
	}
	identity, err := ref.Identity(baseDir)
	if err != nil {
		return "unsupported:" + strings.TrimSpace(raw)
	}
	return identity
}

func noMatchingPackageMessage(cwd, source string, pkgs []configuredPackage) string {
	if suggestion := findSuggestedConfiguredSource(source, pkgs); suggestion != "" {
		return fmt.Sprintf("No matching package found for %s. Did you mean %s?", source, suggestion)
	}
	return fmt.Sprintf("No matching package found for %s", source)
}

func findSuggestedConfiguredSource(source string, pkgs []configuredPackage) string {
	trimmed := strings.TrimSpace(source)
	for _, pkg := range pkgs {
		sourceStr := pkg.Source.Source
		if name, spec, ok := parseNpmSource(sourceStr); ok {
			if trimmed == name || trimmed == spec {
				return sourceStr
			}
			continue
		}
		if git, ok := parseGitPackageSource(sourceStr); ok {
			shorthand := git.host + "/" + git.path
			if trimmed == shorthand {
				return sourceStr
			}
			if git.ref != "" && trimmed == shorthand+"@"+git.ref {
				return sourceStr
			}
		}
	}
	return ""
}

func parseNpmSource(source string) (name, spec string, ok bool) {
	if !strings.HasPrefix(source, "npm:") {
		return "", "", false
	}
	spec = strings.TrimSpace(strings.TrimPrefix(source, "npm:"))
	name, _ = parseNpmSpec(spec)
	return name, spec, name != ""
}

func parseNpmInstallRef(source string) (sourceref.Ref, error) {
	ref, err := sourceref.Parse(source, sourceref.Options{Bare: sourceref.BareNPM})
	if err != nil || ref.Kind != sourceref.KindNPM {
		return sourceref.Ref{}, fmt.Errorf("invalid npm package source: %s", source)
	}
	return ref, nil
}

func parseGitPackageSource(source string) (gitPackageSource, bool) {
	ref, err := sourceref.Parse(source, sourceref.Options{Bare: sourceref.BareReject})
	if err != nil || ref.Kind != sourceref.KindGit {
		return gitPackageSource{}, false
	}
	return gitPackageSource{
		host: ref.GitHost, path: ref.GitPath, ref: ref.GitRef, pinned: ref.GitRef != "",
	}, true
}

func installManagedNPM(cwd string, sm *codingagent.SettingsManager, source string, local bool) error {
	ref, err := parseNpmInstallRef(source)
	if err != nil {
		return err
	}
	installRoot := npmInstallRoot(cwd, sm, ref, local)
	if err := ensureManagedPackageRoot(installRoot); err != nil {
		return err
	}
	command := defaultNpmCommand(sm)
	args := append([]string{}, command[1:]...)
	args = append(args, npmInstallArgs(npmCommandName(command), ref.Locator, installRoot, ref.NPMRegistry)...)
	return runPackageProcess("", command[0], args...)
}

func uninstallManagedNPM(cwd string, sm *codingagent.SettingsManager, source string, local bool) error {
	ref, err := parseNpmInstallRef(source)
	if err != nil {
		return err
	}
	installRoot := npmInstallRoot(cwd, sm, ref, local)
	if _, err := os.Stat(installRoot); os.IsNotExist(err) {
		return nil
	}
	command := defaultNpmCommand(sm)
	args := append([]string{}, command[1:]...)
	if npmCommandName(command) == "bun" {
		args = append(args, "uninstall", ref.NPMName, "--cwd", installRoot)
	} else {
		args = append(args, "uninstall", ref.NPMName, "--prefix", installRoot)
		if npmCommandName(command) != "pnpm" {
			args = append(args, "--legacy-peer-deps")
		}
	}
	if ref.NPMRegistry != "" {
		args = append(args, "--registry", ref.NPMRegistry)
	}
	return runPackageProcess("", command[0], args...)
}

func npmInstallArgs(manager, spec, installRoot, registry string) []string {
	var args []string
	switch manager {
	case "bun":
		args = []string{"install", spec, "--cwd", installRoot, "--omit=peer"}
	case "pnpm":
		args = []string{
			"install", spec, "--prefix", installRoot,
			"--config.auto-install-peers=false",
			"--config.strict-peer-dependencies=false",
			"--config.strict-dep-builds=false",
		}
	default:
		args = []string{"install", spec, "--prefix", installRoot, "--legacy-peer-deps"}
	}
	if registry != "" {
		args = append(args, "--registry", registry)
	}
	return args
}

func ensureManagedPackageRoot(root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	codingagent.MarkPathIgnoredByCloudSync(root)
	ignorePath := filepath.Join(root, ".gitignore")
	if _, err := os.Stat(ignorePath); os.IsNotExist(err) {
		if err := os.WriteFile(ignorePath, []byte("*\n!.gitignore\n"), 0o644); err != nil {
			return err
		}
	}
	packageJSON := filepath.Join(root, "package.json")
	if _, err := os.Stat(packageJSON); os.IsNotExist(err) {
		if err := os.WriteFile(packageJSON, []byte("{\n  \"name\": \"pi-extensions\",\n  \"private\": true\n}\n"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func installManagedGit(cwd string, sm *codingagent.SettingsManager, source string, local bool) (err error) {
	ref, err := sourceref.Parse(source, sourceref.Options{Bare: sourceref.BareReject})
	if err != nil || ref.Kind != sourceref.KindGit {
		return fmt.Errorf("invalid Git package source: %s", source)
	}
	checkout, err := gitCheckoutPath(cwd, source, local)
	if err != nil {
		return err
	}
	root := codingagent.GitInstallRoot(cwd, sm.AgentDir(), local)
	packageRoot, err := gitInstallPath(cwd, source, local)
	if err != nil {
		return err
	}
	return installGitCheckout(sm, ref, checkout, packageRoot, root)
}

// installGitCheckout shares update, dependency repair, and failed-clone cleanup between installed and temporary sources. An empty root leaves temporary-cache parents intact, as Pi does.
func installGitCheckout(sm *codingagent.SettingsManager, ref sourceref.Ref, checkout, packageRoot, root string) (err error) {
	if _, statErr := os.Stat(checkout); statErr == nil {
		if err := requireGitSubdirectoryWithinCheckout(checkout, packageRoot); err != nil {
			return err
		}
		target := gitUpdateTarget{ref: "FETCH_HEAD", fetchArgs: []string{"fetch", "origin", ref.GitRef}}
		if ref.GitRef == "" {
			target, err = getLocalGitUpdateTarget(checkout)
			if err != nil {
				return err
			}
		}
		return ensureGitRef(checkout, packageRoot, sm, target)
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	if root != "" {
		if err := ensureManagedCheckoutRoot(root); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(checkout), 0o755); err != nil {
		return err
	}
	if err := removeGitUpdateMarker(gitUpdateMarkerPath(checkout)); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.RemoveAll(checkout), pruneEmptyGitParents(checkout, root))
		}
	}()
	repo := ref.GitRepo
	if !strings.Contains(repo, "://") && !strings.HasPrefix(repo, "git@") {
		repo = "https://" + repo
	}
	if err := runPackageProcess("", "git", "clone", gitCloneRepo(runtime.GOOS, repo), checkout); err != nil {
		return err
	}
	if ref.GitRef != "" {
		if err := runPackageProcess(checkout, "git", "checkout", ref.GitRef); err != nil {
			return err
		}
	}
	info, err := os.Stat(packageRoot)
	if err != nil {
		return fmt.Errorf("Git package subdirectory %q does not exist in %s: %w", ref.GitSubdir, ref.GitRepo, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("Git package subdirectory %q in %s is not a directory", ref.GitSubdir, ref.GitRepo)
	}
	if err := requireGitSubdirectoryWithinCheckout(checkout, packageRoot); err != nil {
		return err
	}
	return installGitDependencies(packageRoot, sm)
}

func gitCheckoutPath(cwd, source string, local bool) (string, error) {
	ref, err := sourceref.Parse(source, sourceref.Options{Bare: sourceref.BareReject})
	if err != nil || ref.Kind != sourceref.KindGit {
		return "", fmt.Errorf("invalid Git package source: %s", source)
	}
	root := codingagent.GitInstallRoot(cwd, codingagent.AgentDir(), local)
	relative, err := gitCheckoutRelative(runtime.GOOS, root, ref)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, relative), nil
}

// gitCheckoutRelative is a Git source's checkout directory below the install
// root: its host, then its repository path. Windows cannot name a directory
// with ':', so there a file:// source's leading drive (C:) becomes the segment
// C, and any other segment containing ':' is refused, since NTFS reads
// name:stream as an alternate data stream.
func gitCheckoutRelative(goos, root string, ref sourceref.Ref) (string, error) {
	segments := append([]string{ref.GitHost}, strings.Split(ref.GitPath, "/")...)
	if goos == "windows" {
		// pig additive (D18): a Windows file URL's drive is a checkout segment.
		if strings.HasPrefix(strings.ToLower(ref.GitRepo), "file://") && isDriveSegment(segments[1]) {
			segments[1] = segments[1][:1]
		}
		for _, segment := range segments {
			if strings.Contains(segment, ":") {
				return "", fmt.Errorf("Refusing to use path outside package install root: %s", filepath.Join(append([]string{root}, segments...)...))
			}
		}
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved, err := resolveManagedPackagePath(absoluteRoot, segments...)
	if err != nil {
		return "", err
	}
	return filepath.Rel(absoluteRoot, resolved)
}

// gitCloneRepo is the URL git clones for repo. Git for Windows reads
// file://localhost/C:/... as the UNC path //localhost/C:/..., so there a
// localhost file URL with a drive becomes the equivalent file:///C:/....
func gitCloneRepo(goos, repo string) string {
	const localhost = "file://localhost/"
	if goos != "windows" || len(repo) < len(localhost) || !strings.EqualFold(repo[:len(localhost)], localhost) {
		return repo
	}
	rest := repo[len(localhost):]
	drive, _, _ := strings.Cut(rest, "/")
	if !isDriveSegment(drive) {
		return repo
	}
	return "file:///" + rest
}

// isDriveSegment reports whether segment is a Windows drive such as C:.
func isDriveSegment(segment string) bool {
	return len(segment) == 2 && segment[1] == ':' && ('A' <= segment[0] && segment[0] <= 'Z' || 'a' <= segment[0] && segment[0] <= 'z')
}

func gitInstallPath(cwd, source string, local bool) (string, error) {
	ref, err := sourceref.Parse(source, sourceref.Options{Bare: sourceref.BareReject})
	if err != nil || ref.Kind != sourceref.KindGit {
		return "", fmt.Errorf("invalid Git package source: %s", source)
	}
	checkout, err := gitCheckoutPath(cwd, source, local)
	if err != nil {
		return "", err
	}
	if ref.GitSubdir == "" {
		return checkout, nil
	}
	return filepath.Join(checkout, filepath.FromSlash(ref.GitSubdir)), nil
}

func requireGitSubdirectoryWithinCheckout(checkout, packageRoot string) error {
	resolvedCheckout, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		return fmt.Errorf("resolve Git checkout %s: %w", checkout, err)
	}
	resolvedPackage, err := filepath.EvalSymlinks(packageRoot)
	if err != nil {
		return fmt.Errorf("resolve Git package root %s: %w", packageRoot, err)
	}
	relative, err := filepath.Rel(resolvedCheckout, resolvedPackage)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("Git package subdirectory %s resolves outside checkout %s", packageRoot, checkout)
	}
	return nil
}

func ensureManagedCheckoutRoot(root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	codingagent.MarkPathIgnoredByCloudSync(root)
	ignorePath := filepath.Join(root, ".gitignore")
	if _, err := os.Stat(ignorePath); os.IsNotExist(err) {
		return os.WriteFile(ignorePath, []byte("*\n!.gitignore\n"), 0o644)
	}
	return nil
}

func detectSourceKind(source string) string {
	// Pi package-manager.ts:1446-1470 treats unprefixed sources as local, whether or not the path exists.
	ref, err := sourceref.Parse(source, sourceref.Options{Bare: sourceref.BareLocal, AllowContributed: true})
	if err != nil {
		return "unsupported"
	}
	switch ref.Kind {
	case sourceref.KindNPM:
		return "npm"
	case sourceref.KindGit:
		return "git"
	case sourceref.KindLocal:
		return "local"
	case sourceref.KindContributed:
		if installresolver.SupportsSourceScheme(ref.Scheme) {
			return ref.Scheme
		}
	}
	return "unsupported"
}

func runCmd(name string, args ...string) (string, error) {
	return runCmdInDir("", name, args...)
}

// runCmdInDir is Pi's runCommandSync. It starts the command as upstream's spawnProcess does, so a Windows .cmd shim such as npm.cmd receives its arguments exactly, and returns the trimmed stdout, or stderr when stdout is empty, so a warning on stderr cannot corrupt a reported path.
func runCmdInDir(dir, name string, args ...string) (string, error) {
	cmd := crossspawn.Command(context.Background(), dir, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		detail := stderr.String()
		if detail == "" {
			detail = stdout.String()
		}
		return "", fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(detail))
	}
	output := stdout.String()
	if output == "" {
		output = stderr.String()
	}
	return strings.TrimSpace(output), nil
}
