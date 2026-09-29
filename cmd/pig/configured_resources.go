package main

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"golang.org/x/term"

	"github.com/MichaelKinsy/PiG/coding/extension/host/subprocess"
	extsource "github.com/MichaelKinsy/PiG/coding/extension/source"
	"github.com/MichaelKinsy/PiG/coding/packagecontent"
	"github.com/MichaelKinsy/PiG/internal/codingagent"
	"github.com/MichaelKinsy/PiG/internal/resolvepath"
)

func configuredExtensionResolver(resolvers []extsource.ResolveFunc) extsource.ResolveFunc {
	if len(resolvers) > 0 && resolvers[0] != nil {
		return resolvers[0]
	}
	return extsource.Resolve
}

func collectStartupThemePaths(cwd, agentDir string, sm *codingagent.SettingsManager) []string {
	global := sm.GetGlobalSettings()
	paths := collectTopLevelResourcePaths(filepath.Join(agentDir, "themes"), global.Themes, "themes")
	for _, pkg := range global.Packages {
		root := installedPathForConfiguredSource(cwd, sm, pkg.Source, false)
		if configuredPackageNeedsInstall(configuredPackage{Source: pkg, InstalledPath: root}) {
			continue
		}
		items, err := collectPackageResourceItems(root, configuredPackage{Source: pkg, Scope: "user", InstalledPath: root}, false)
		if err != nil {
			continue
		}
		for _, item := range items {
			if item.ResourceType == "themes" && item.Enabled {
				paths = append(paths, item.Path)
			}
		}
	}
	return dedupStrings(paths)
}

func projectResourceRoot(cwd string) (string, bool) {
	root := codingagent.ProjectConfigDir(cwd)
	return root, !samePath(root, codingagent.ConfigRoot())
}

func collectPromptPaths(cwd, agentDir string, sm *codingagent.SettingsManager, flags CLIFlags, projectTrusted bool, resolvers ...extsource.ResolveFunc) []string {
	paths := make([]string, 0)
	// Pi keeps the first same-name prompt after ordering CLI, project, user,
	// then Package resources.
	for _, promptPath := range flags.PromptTemplates {
		paths = append(paths, collectResourceFilesFromPaths([]string{resolveSettingsPath(cwd, promptPath)}, "prompts")...)
	}
	if !flags.NoPromptTemplates {

		if projectRoot, ok := projectResourceRoot(cwd); projectTrusted && ok {
			paths = append(paths, collectTopLevelResourcePaths(filepath.Join(projectRoot, "prompts"), sm.GetProjectSettings().Prompts, "prompts")...)
		}
		paths = append(paths, collectTopLevelResourcePaths(filepath.Join(agentDir, "prompts"), sm.GetGlobalSettings().Prompts, "prompts")...)
		paths = append(paths, collectPackagePromptPaths(cwd, sm, resolvers...)...)
	}
	return dedupStrings(paths)
}

// collectThemePaths lists theme paths in upstream precedence order, the first
// theme of a name winning: --theme paths, project, user, then Packages.
func collectThemePaths(cwd, agentDir string, sm *codingagent.SettingsManager, flags CLIFlags, projectTrusted bool, resolvers ...extsource.ResolveFunc) []string {
	paths := make([]string, 0)
	for _, p := range flags.Themes {
		paths = append(paths, resolveSettingsPath(cwd, p))
	}
	if !flags.NoThemes {
		if projectRoot, ok := projectResourceRoot(cwd); projectTrusted && ok {
			paths = append(paths, collectTopLevelResourcePaths(filepath.Join(projectRoot, "themes"), sm.GetProjectSettings().Themes, "themes")...)
		}
		paths = append(paths, collectTopLevelResourcePaths(filepath.Join(agentDir, "themes"), sm.GetGlobalSettings().Themes, "themes")...)
		paths = append(paths, collectPackageThemePaths(cwd, sm, resolvers...)...)
	}
	return dedupStrings(paths)
}

// packageManagerHomeDir prefers nonempty HOME on every platform. Otherwise it uses the platform home variable, including an explicit empty value, or the OS user database when that variable is absent.
// Ports packages/coding-agent/src/core/package-manager.ts:getHomeDir.
func packageManagerHomeDir() string {
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	key := "HOME"
	if runtime.GOOS == "windows" {
		key = "USERPROFILE"
	}
	if home, present := os.LookupEnv(key); present {
		return home
	}
	if current, err := user.Current(); err == nil {
		return current.HomeDir
	}
	return ""
}

// collectSkillInputs appends --skill paths after resolved project, user, and Package skills, as DefaultResourceLoader.reload does. --no-skills keeps the explicit paths alone.
// Ports packages/coding-agent/src/core/resource-loader.ts
func collectSkillInputs(cwd, agentDir string, sm *codingagent.SettingsManager, flags CLIFlags, ambientScopes *[]string, resolvers ...extsource.ResolveFunc) []string {
	var cliInputs []string
	for _, p := range flags.Skills {
		cliInputs = append(cliInputs, collectResourceFilesFromPaths([]string{resolveSettingsPath(cwd, p)}, "skills")...)
	}
	if flags.NoSkills {
		return dedupStrings(cliInputs)
	}

	inputs := make([]string, 0)
	// Resolved paths follow resourcePrecedenceRank; explicit --skill paths are additionalSkillPaths, not cliEnabledSkills from -e Packages.
	projectRoot, projectResourcesEnabled := projectResourceRoot(cwd)
	if ambientSourceEnabled(ambientScopes, "workspace") && projectResourcesEnabled {
		projectSettings := sm.GetProjectSettings().Skills
		inputs = append(inputs, resolveConfiguredResourceEntries(projectSettings, projectRoot, "skills")...)
		projectAuto := collectAutoDiscoveredResourcePaths(filepath.Join(projectRoot, "skills"), "skills")
		inputs = append(inputs, filterAutoDiscoveredPaths(projectAuto, projectSettings, projectRoot, "skills")...)

		userAgentsSkills := filepath.Join(packageManagerHomeDir(), ".agents", "skills")
		for _, dir := range discoverAncestorAgentsSkillDirs(cwd) {
			abs, _ := filepath.Abs(dir)
			uabs, _ := filepath.Abs(userAgentsSkills)
			if abs == uabs {
				continue
			}
			inputs = append(inputs, filterAutoDiscoveredPaths(discoverSkillDir(dir), projectSettings, filepath.Dir(dir), "skills")...)
		}
	}

	if ambientSourceEnabled(ambientScopes, "user") {
		userSettings := sm.GetGlobalSettings().Skills
		inputs = append(inputs, resolveConfiguredResourceEntries(userSettings, agentDir, "skills")...)
		userAuto := collectAutoDiscoveredResourcePaths(filepath.Join(agentDir, "skills"), "skills")
		inputs = append(inputs, filterAutoDiscoveredPaths(userAuto, userSettings, agentDir, "skills")...)
		userAgentsSkills := filepath.Join(packageManagerHomeDir(), ".agents", "skills")
		inputs = append(inputs, filterAutoDiscoveredPaths(discoverSkillDir(userAgentsSkills), userSettings, filepath.Dir(userAgentsSkills), "skills")...)
	}

	inputs = append(inputs, collectPackageSkillPaths(cwd, sm, ambientScopes, resolvers...)...)
	return dedupStrings(append(inputs, cliInputs...))
}

func collectExtensionConfigs(cwd, agentDir string, sm *codingagent.SettingsManager, flags CLIFlags, ambientScopes *[]string, resolvers ...extsource.ResolveFunc) []subprocess.ExtConfig {
	// Upstream resource-loader.ts loads -e paths first, then the resolved
	// paths in package-manager.ts resourcePrecedenceRank order: project
	// settings entries, project auto-discovery, user settings entries, user
	// auto-discovery, then Packages. It reports a missing -e path after
	// loading. --no-extensions keeps only the -e paths.
	configs := make([]subprocess.ExtConfig, 0)
	var missing []subprocess.ExtConfig
	for _, p := range flags.Extensions {
		resolved, err := resolveCLIExtensionSource(cwd, agentDir, sm, p, nil)
		if err != nil {
			missing = append(missing, subprocess.UnresolvedExtConfig(p, err))
			continue
		}
		if resolved == "" {
			continue
		}
		loaded := cliExtensionConfigs
		if kind := detectSourceKind(p); kind == "git" || kind == "npm" {
			// Pi package-manager.ts:1293-1307 collects an npm or git -e checkout only as a Package; unlike a local directory (:1346-1351), it never loads the checkout root itself.
			loaded = func(root string, resolvers ...extsource.ResolveFunc) []subprocess.ExtConfig {
				return withCLISourceInfo(packageExtensionConfigs(root, nil, resolvers...))
			}
		}
		for _, config := range loaded(resolved, resolvers...) {
			if _, absent := errors.AsType[extensionPathMissingError](config.ResolveError()); absent {
				missing = append(missing, config)
				continue
			}
			configs = append(configs, config)
		}
	}
	if !flags.NoExtensions {
		if projectRoot, ok := projectResourceRoot(cwd); ambientSourceEnabled(ambientScopes, "workspace") && ok {
			configs = append(configs, collectTopLevelExtensionConfigs(filepath.Join(projectRoot, "extensions"), sm.GetProjectSettings().Extensions, "project", resolvers...)...)
		}
		if ambientSourceEnabled(ambientScopes, "user") {
			configs = append(configs, collectTopLevelExtensionConfigs(filepath.Join(agentDir, "extensions"), sm.GetGlobalSettings().Extensions, "user", resolvers...)...)
		}
		configs = append(configs, collectPackageExtensionConfigs(cwd, sm, ambientScopes, resolvers...)...)
	}
	return mergeExtConfigs(uniqueExtensionPaths(append(configs, missing...)))
}

// uniqueExtensionPaths retains the first selected alias of each canonical extension source before any factory runs.
func uniqueExtensionPaths(configs []subprocess.ExtConfig) []subprocess.ExtConfig {
	type sourceKey struct{ path, module, pkg, factory string }
	seen := make(map[sourceKey]struct{}, len(configs))
	out := make([]subprocess.ExtConfig, 0, len(configs))
	for _, config := range configs {
		path := config.Source
		if path == "" {
			path = config.Path
		}
		if path == "" {
			out = append(out, config)
			continue
		}
		if absolute, err := filepath.Abs(path); err == nil {
			path = absolute
		}
		// pig additive (D20): distinct native factories in one source module retain their separate registrations.
		key := sourceKey{codingagent.CanonicalizePath(path), config.ModulePath, config.Package, config.Factory}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, config)
	}
	return out
}

// cliExtensionConfigs resolves one -e path. Like upstream resource-loader.ts,
// a missing local path is a load failure: "Extension path does not exist".
// A directory that is not itself an extension loads the extensions inside it.
// Each loaded extension carries upstream's CLI provenance.
func cliExtensionConfigs(resolved string, resolvers ...extsource.ResolveFunc) []subprocess.ExtConfig {
	info, err := os.Stat(resolved)
	if os.IsNotExist(err) {
		return []subprocess.ExtConfig{subprocess.UnresolvedExtConfig(resolved, extensionPathMissingError{path: resolved})}
	}
	if err == nil && info.IsDir() && packagecontent.HasPiManifest(resolved) {
		// Upstream resolves a directory with a "pi" manifest as a Package
		// (resolveLocalExtensionSource, collectPackageResources): each entry
		// its manifest yields is its own extension, and a manifest that yields
		// none loads nothing.
		return withCLISourceInfo(packageExtensionConfigs(resolved, nil, resolvers...))
	}
	configs := pathToExtConfigs(resolved, resolvers...)
	if len(configs) > 0 && configs[0].ResolveError() == nil {
		return withCLISourceInfo(configs)
	}
	var expanded []subprocess.ExtConfig
	for _, path := range collectResourceFilesFromPaths([]string{resolved}, "extensions") {
		expanded = append(expanded, pathToExtConfigs(path, resolvers...)...)
	}
	if len(expanded) == 0 {
		return configs
	}
	return withCLISourceInfo(expanded)
}

// withCLISourceInfo stamps the SourceInfo upstream records for an extension
// named on the command line, {source: "cli", scope: "temporary", origin:
// "top-level"} with no baseDir (resource-loader.ts "Add CLI paths
// metadata"), onto each config. Its tools and commands report it.
func withCLISourceInfo(configs []subprocess.ExtConfig) []subprocess.ExtConfig {
	for i := range configs {
		path := configs[i].Source
		if path == "" {
			path = configs[i].Path
		}
		configs[i].SourceInfo = codingagent.CLISourceInfo(path)
	}
	return configs
}

// collectTopLevelExtensionConfigs resolves a scope's settings entries, then
// its auto-discovered extensions, stamping the SourceInfo upstream
// package-manager.ts records for each: {source: "local"} without a baseDir for
// a settings entry, {source: "auto", baseDir: <config dir>} for a discovered
// one.
func collectTopLevelExtensionConfigs(autoDir string, entries []string, scope string, resolvers ...extsource.ResolveFunc) []subprocess.ExtConfig {
	baseDir := filepath.Dir(autoDir)
	automatic := filterAutoDiscoveredPaths(collectAutoDiscoveredResourcePaths(autoDir, "extensions"), entries, baseDir, "extensions")
	configs := make([]subprocess.ExtConfig, 0, len(automatic)+len(entries))
	// Settings entries rank before auto-discovery in the same scope.
	for _, path := range resolveConfiguredResourceEntries(entries, baseDir, "extensions") {
		for _, config := range pathToExtConfigs(path, resolvers...) {
			config.SourceInfo = codingagent.PiSourceInfo{Path: path, Source: "local", Scope: scope, Origin: "top-level"}
			configs = append(configs, config)
		}
	}
	for _, path := range automatic {
		selected := path
		if name := filepath.Base(path); (name == "index.ts" || name == "index.js") && !extsource.NodeDeclaresExtensions(filepath.Dir(path)) {
			selected = filepath.Dir(path)
		}
		// Upstream loads and names the discovered entry file; PiG loads the
		// selected directory and names the entry file in load errors.
		for _, config := range pathToExtConfigs(selected, resolvers...) {
			config.SourceInfo = codingagent.PiSourceInfo{Path: path, Source: "auto", Scope: scope, Origin: "top-level", BaseDir: baseDir}
			configs = append(configs, config.SelectedAs(path))
		}
	}
	return configs
}

// collectTopLevelResourcePaths lists a scope's settings entries, then its
// auto-discovered resources, as upstream package-manager.ts
// resourcePrecedenceRank orders them.
func collectTopLevelResourcePaths(autoDir string, entries []string, kind string) []string {
	auto := collectAutoDiscoveredResourcePaths(autoDir, kind)
	baseDir := filepath.Dir(autoDir)
	explicit := resolveConfiguredResourceEntries(entries, baseDir, kind)
	auto = filterAutoDiscoveredPaths(auto, entries, baseDir, kind)
	return append(explicit, auto...)
}

func resolveConfiguredResourceEntries(entries []string, baseDir string, kind string) []string {
	return packagecontent.ResolveConfigured(entries, baseDir, packagecontent.Kind(kind))
}

func filterAutoDiscoveredPaths(paths, overrides []string, baseDir string, kind string) []string {
	return packagecontent.FilterAutomatic(paths, overrides, baseDir, packagecontent.Kind(kind))
}

func collectAutoDiscoveredResourcePaths(dir string, kind string) []string {
	return packagecontent.DiscoverAutomatic(dir, packagecontent.Kind(kind))
}

func collectResourceFilesFromPaths(paths []string, kind string) []string {
	return packagecontent.Collect(paths, packagecontent.Kind(kind))
}

func splitResourcePatterns(entries []string) (plain, patterns []string) {
	return packagecontent.SplitPatterns(entries)
}

func applyResourcePatterns(allPaths, patterns []string, baseDir, kind string) []string {
	return packagecontent.ApplyPatterns(allPaths, patterns, baseDir, packagecontent.Kind(kind))
}

func isEnabledByOverrides(filePath string, patterns []string, baseDir, kind string) bool {
	return packagecontent.EnabledByOverrides(filePath, patterns, baseDir, packagecontent.Kind(kind))
}

func configuredPackageFilters(source codingagent.PackageSource) map[packagecontent.Kind][]string {
	return map[packagecontent.Kind][]string{
		packagecontent.Extensions: source.Extensions,
		packagecontent.Skills:     source.Skills,
		packagecontent.Prompts:    source.Prompts,
		packagecontent.Themes:     source.Themes,
	}
}

func effectiveConfiguredPackageFilters(pkg configuredPackage, resolvers ...extsource.ResolveFunc) (map[packagecontent.Kind][]string, error) {
	filters := configuredPackageFilters(pkg.Source)
	if !isProjectPackageDelta(pkg.Source) {
		return filters, nil
	}
	items, err := collectPackageResourceItems(pkg.InstalledPath, pkg, true, resolvers...)
	if err != nil {
		return nil, err
	}
	for kind := range filters {
		filters[kind] = []string{}
	}
	for _, item := range items {
		if !item.Enabled {
			continue
		}
		path := item.Path
		if item.ResourceType == "skills" {
			path = packagecontent.SkillFile(path)
		}
		relative, err := filepath.Rel(pkg.InstalledPath, path)
		if err != nil {
			return nil, err
		}
		kind := packagecontent.Kind(item.ResourceType)
		filters[kind] = append(filters[kind], filepath.ToSlash(relative))
	}
	return filters, nil
}

// extensionLoadFailureHint mirrors upstream main.ts EXTENSION_LOAD_FAILURE_HINT.
const extensionLoadFailureHint = `Hint: Start without extensions using "` + codingagent.AppName + ` -ne".`

// validateConfiguredPackagesForStartup returns a Package diagnostic without
// making startup fatal. Manifest parsing matches discovery. Extension failures
// belong to the final extension load, which reports every failure together.
// Scopes that do not load extensions skip source resolution.
func validateConfiguredPackagesForStartup(cwd string, sm *codingagent.SettingsManager, loadsExtensions func(scope string) bool, resolvers ...extsource.ResolveFunc) error {
	for _, pkg := range resolvedConfiguredPackageSources(cwd, sm, true) {
		if configuredPackageNeedsInstall(pkg) {
			continue
		}
		filters, err := effectiveConfiguredPackageFilters(pkg, resolvers...)
		if err != nil {
			return invalidConfiguredPackageError(pkg, err)
		}
		if !loadsExtensions(pkg.Scope) {
			filters[packagecontent.Extensions] = []string{}
		}
		// A declared member that matches nothing is skipped, as upstream's
		// package manager skips it; the rest of the Package still loads.
		if _, _, _, err := packagecontent.ValidateConfiguredForStartupWithResolver(pkg.InstalledPath, filters, configuredExtensionResolver(resolvers)); err != nil {
			return invalidConfiguredPackageError(pkg, err)
		}
	}
	return nil
}

type extensionPathMissingError struct{ path string }

func (e extensionPathMissingError) Error() string {
	return "Extension path does not exist: " + e.path
}

// extensionLoadFailureDiagnostic formats one failed extension as upstream
// main.ts does: `Failed to load extension "<path>": <loader error>`, where the
// loader error is `Failed to load extension: <message>` except for a missing
// -e path. An extension runtime that reported its own loader error (Node's
// cell.mjs) already words it as Pi's loader does.
func extensionLoadFailureDiagnostic(path string, err error) codingagent.AgentSessionRuntimeDiagnostic {
	message := "Failed to load extension: " + err.Error()
	if _, missing := errors.AsType[extensionPathMissingError](err); missing {
		message = err.Error()
	}
	if factoryErr, reported := errors.AsType[*subprocess.FactoryLoadError](err); reported {
		message = factoryErr.Error()
	}
	return codingagent.AgentSessionRuntimeDiagnostic{Type: "error", Message: fmt.Sprintf(`Failed to load extension "%s": %s`, path, message)}
}

// extensionLoadDiagnostics formats host load errors. An error without an
// extension path, such as an embedded cell failure, keeps its own text.
func extensionLoadDiagnostics(errs []error) []codingagent.AgentSessionRuntimeDiagnostic {
	diagnostics := make([]codingagent.AgentSessionRuntimeDiagnostic, 0, len(errs))
	for _, err := range errs {
		if loadErr, ok := errors.AsType[*subprocess.ExtensionLoadError](err); ok && loadErr.Path != "" {
			diagnostics = append(diagnostics, extensionLoadFailureDiagnostic(loadErr.Path, loadErr.Err))
			continue
		}
		diagnostics = append(diagnostics, codingagent.AgentSessionRuntimeDiagnostic{Type: "error", Message: "Failed to load extension: " + err.Error()})
	}
	return diagnostics
}

// extensionConflictDiagnostics formats tool and flag conflicts as upstream
// main.ts formats its extension errors: `Failed to load extension "<path>":
// <conflict>`.
func extensionConflictDiagnostics(conflicts []codingagent.ExtensionConflict) []codingagent.AgentSessionRuntimeDiagnostic {
	diagnostics := make([]codingagent.AgentSessionRuntimeDiagnostic, 0, len(conflicts))
	for _, conflict := range conflicts {
		diagnostics = append(diagnostics, codingagent.AgentSessionRuntimeDiagnostic{Type: "error", Message: fmt.Sprintf(`Failed to load extension "%s": %s`, conflict.Path, conflict.Message)})
	}
	return diagnostics
}

// reportExtensionLoadFailures mirrors upstream main.ts on extension load
// errors: it reports the startup diagnostics, then the -ne hint in yellow.
func reportExtensionLoadFailures(diagnostics []codingagent.AgentSessionRuntimeDiagnostic) {
	codingagent.ReportDiagnostics(codingagent.DeduplicateDiagnostics(diagnostics))
	hint := extensionLoadFailureHint
	if term.IsTerminal(int(os.Stderr.Fd())) {
		hint = "\x1b[33m" + hint + "\x1b[39m"
	}
	fmt.Fprintln(os.Stderr, hint)
}

func invalidConfiguredPackageError(pkg configuredPackage, err error) error {
	return fmt.Errorf("%s Package %q at %s is invalid: %w", pkg.Scope, pkg.Source.Source, pkg.InstalledPath, err)
}

func collectPackagePromptPaths(cwd string, sm *codingagent.SettingsManager, resolvers ...extsource.ResolveFunc) []string {
	return collectEnabledPackagePaths(cwd, sm, packagecontent.Prompts, nil, resolvers...)
}

func collectPackageThemePaths(cwd string, sm *codingagent.SettingsManager, resolvers ...extsource.ResolveFunc) []string {
	return collectEnabledPackagePaths(cwd, sm, packagecontent.Themes, nil, resolvers...)
}

func collectPackageSkillPaths(cwd string, sm *codingagent.SettingsManager, ambientScopes *[]string, resolvers ...extsource.ResolveFunc) []string {
	return collectEnabledPackagePaths(cwd, sm, packagecontent.Skills, ambientScopes, resolvers...)
}

func collectPackageExtensionConfigs(cwd string, sm *codingagent.SettingsManager, ambientScopes *[]string, resolvers ...extsource.ResolveFunc) []subprocess.ExtConfig {
	items, _ := collectResolvedPackageResourceItems(cwd, sm, ambientScopes, false, resolvers...)
	var configs []subprocess.ExtConfig
	for _, item := range items {
		if item.ResourceType != "extensions" || !item.Enabled {
			continue
		}
		for _, config := range pathToExtConfigs(item.Path, resolvers...) {
			config.SourceInfo = codingagent.PiSourceInfo{Path: item.Path, Source: item.Source, Scope: item.Scope, Origin: "package", BaseDir: item.BaseDir}
			configs = append(configs, config)
		}
	}
	return configs
}

// packageExtensionConfigs resolves each extension entry the Package at root
// exposes and filter enables as its own extension, as upstream loads every
// file its manifest entries collect.
func packageExtensionConfigs(root string, filter []string, resolvers ...extsource.ResolveFunc) []subprocess.ExtConfig {
	resources, err := packagecontent.Discover(root)
	if err != nil {
		return nil
	}
	var configs []subprocess.ExtConfig
	for _, src := range resources.ExtensionEntries {
		rel, _ := filepath.Rel(root, src)
		if !packagecontent.ResourceEnabled(rel, filter) {
			continue
		}
		name, err := packagecontent.PublicName(packagecontent.Extensions, src, "")
		if err != nil {
			continue
		}
		config, _, err := subprocess.ResolveExtConfigWithResolver(src, name, configuredExtensionResolver(resolvers))
		if err != nil {
			config = subprocess.UnresolvedExtConfig(src, err)
		}
		configs = append(configs, config)
	}
	return configs
}

func packageScopeEnabled(ambientScopes *[]string, scope string) bool {
	if scope == "project" {
		return ambientSourceEnabled(ambientScopes, "workspace")
	}
	return ambientSourceEnabled(ambientScopes, "user")
}

func ambientSourceEnabled(ambientScopes *[]string, source string) bool {
	return ambientScopes == nil || slices.Contains(*ambientScopes, source)
}

func trustedAmbientScopes(scopes *[]string) *[]string {
	if scopes == nil {
		userOnly := []string{"user"}
		return &userOnly
	}
	trusted := make([]string, 0, len(*scopes))
	for _, scope := range *scopes {
		if scope != "workspace" {
			trusted = append(trusted, scope)
		}
	}
	return &trusted
}

func configuredPackagesForResolution(cwd string, sm *codingagent.SettingsManager) []configuredPackage {
	return resolvedConfiguredPackageSources(cwd, sm, false)
}

// resolvedConfiguredPackageSources keeps separate project deltas for resource accumulation; installation consumers share the inherited user package.
func resolvedConfiguredPackageSources(cwd string, sm *codingagent.SettingsManager, resourceEntries bool) []configuredPackage {
	global := sm.GetGlobalSettings().Packages
	project := sm.GetProjectSettings().Packages
	globals := make([]configuredPackage, 0, len(global))
	globalByIdentity := make(map[string]int, len(global))
	for _, pkg := range global {
		identity := packageSourceIdentity(settingsBaseDirForManager(sm, false), pkg.Source)
		if _, exists := globalByIdentity[identity]; exists {
			continue
		}
		globalByIdentity[identity] = len(globals)
		globals = append(globals, configuredPackage{
			Source: pkg, Scope: "user", InstalledPath: installedPathForConfiguredSource(cwd, sm, pkg.Source, false),
		})
	}

	projects := make([]configuredPackage, 0, len(project))
	seenProject := make(map[string]struct{}, len(project))
	for i := range project {
		pkg := project[i]
		identity := packageSourceIdentity(settingsBaseDirForManager(sm, true), pkg.Source)
		if _, exists := seenProject[identity]; exists {
			continue
		}
		seenProject[identity] = struct{}{}
		globalIndex, inherited := globalByIdentity[identity]
		if inherited && isProjectPackageDelta(pkg) {
			if resourceEntries {
				projects = append(projects, configuredPackage{Source: pkg, Scope: "project", InstalledPath: globals[globalIndex].InstalledPath, ResolvedSource: globals[globalIndex].Source.Source})
			}
			continue
		}
		if inherited {
			delete(globalByIdentity, identity)
			globals[globalIndex].Source.Source = ""
		}
		projects = append(projects, configuredPackage{
			Source: pkg, Scope: "project", InstalledPath: installedPathForConfiguredSource(cwd, sm, pkg.Source, true),
		})
	}
	out := projects
	for _, pkg := range globals {
		if pkg.Source.Source != "" {
			out = append(out, pkg)
		}
	}
	return out
}

func installedPathForConfiguredSource(cwd string, sm *codingagent.SettingsManager, source string, local bool) string {
	if detectSourceKind(source) != "local" {
		return installedPathForSource(cwd, sm, source, local)
	}
	root, err := resolveLocalPackageRoot(settingsBaseDirForManager(sm, local), source)
	if err != nil {
		return ""
	}
	if _, err := os.Stat(root); err != nil {
		return ""
	}
	return root
}

func isProjectPackageDelta(pkg codingagent.PackageSource) bool {
	return pkg.Autoload != nil && !*pkg.Autoload
}

// resolveSettingsPath is Pi's resolvePath(p, baseDir, { trim: true }) for
// settings entries and already-resolved CLI paths. An invalid file: URL keeps
// the entry joined to baseDir and is silently dropped later, where Pi throws
// (REVIEW-CLIEXT-017 in docs/parity/KNOWN-GAPS-0.3.x.md).
func resolveSettingsPath(baseDir, p string) string {
	if p == "" {
		return p
	}
	if resolved, err := resolvepath.ResolveTrimmed(p, baseDir); err == nil {
		return resolved
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Clean(filepath.Join(baseDir, p))
}

func dedupStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, value := range in {
		key := canonicalStatusPath(value)
		if value != "" && !seen[key] {
			seen[key] = true
			out = append(out, value)
		}
	}
	return out
}

func pathToExtConfig(path string) (subprocess.ExtConfig, bool) {
	configs := pathToExtConfigs(path)
	if len(configs) != 1 || configs[0].ResolveError() != nil {
		return subprocess.ExtConfig{}, false
	}
	return configs[0], true
}

// pathToExtConfigs resolves one extension path. A directory whose source does
// not resolve becomes an unresolved config, so loading reports it as a failed
// extension the way upstream loadExtensions does.
func pathToExtConfigs(path string, resolvers ...extsource.ResolveFunc) []subprocess.ExtConfig {
	if path == "" {
		return nil
	}
	// Use the same conventional factory/standalone resolver as validation and
	// convention-directory discovery.
	config, _, err := subprocess.ResolveExtConfigWithResolver(path, "", configuredExtensionResolver(resolvers))
	if err == nil {
		return []subprocess.ExtConfig{config}
	}
	if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
		return []subprocess.ExtConfig{subprocess.UnresolvedExtConfig(path, err)}
	}
	// Fallback: direct binary or script path that the spec system cannot
	// resolve (e.g. a bare binary path passed via -e /usr/local/bin/my-ext).
	return []subprocess.ExtConfig{{Name: filepath.Base(path), Enabled: true, Path: path}}
}

func mergeExtConfigs(configs []subprocess.ExtConfig) []subprocess.ExtConfig {
	byOrigin := make(map[string]subprocess.ExtConfig, len(configs))
	keys := make([]string, 0, len(configs))
	for _, config := range configs {
		if config.Name == "" {
			base := config.Source
			if base == "" {
				base = config.Path
			}
			config.Name = filepath.Base(strings.TrimSuffix(base, string(filepath.Separator)))
		}
		origin := config.Source
		if origin == "" {
			origin = config.Path
		}
		if absolute, err := filepath.Abs(origin); err == nil {
			origin = absolute
		}
		key := config.Name + "\x00" + filepath.Clean(origin)
		if _, duplicate := byOrigin[key]; !duplicate {
			keys = append(keys, key)
		}
		byOrigin[key] = config
	}
	out := make([]subprocess.ExtConfig, 0, len(keys))
	for _, key := range keys {
		out = append(out, byOrigin[key])
	}
	return out
}

func discoverSkillDir(dir string) []string {
	return packagecontent.DiscoverAgentSkillDirs(dir)
}

// discoverAncestorAgentsSkillDirs walks from startDir up to the git root
// (or filesystem root), collecting .agents/skills/ directories at each level.
// Mirrors upstream collectAncestorAgentsSkillDirs (package-manager.ts).
func discoverAncestorAgentsSkillDirs(startDir string) []string {
	resolved, err := filepath.Abs(startDir)
	if err != nil {
		return nil
	}
	gitRoot := findGitRepoRoot(resolved)

	var dirs []string
	dir := resolved
	for {
		candidate := filepath.Join(dir, ".agents", "skills")
		dirs = append(dirs, candidate)
		if gitRoot != "" && dir == gitRoot {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return dirs
}

// findGitRepoRoot walks up from startDir looking for a .git directory.
// Returns the directory containing .git, or "" if none found.
func findGitRepoRoot(startDir string) string {
	dir := startDir
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
