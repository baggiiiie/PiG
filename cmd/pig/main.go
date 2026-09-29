// pig: Go-native port of pi-coding-agent.
//
// Distinct binary name and config root from upstream pi to avoid PATH
// collisions and shared-config corruption. See codingagent.ConfigRoot()
// for the on-disk layout (defaults to ~/.pig, override with $PIG_HOME).
//
// Usage:
//
//	pig [options] [prompt]
//
// Options:
//
//	--model <provider/model>    Model to use (default: from settings)
//	--agent <name>              Agent definition to load from <config>/agents/
//	--print <prompt>            Print mode: send prompt and print response, then exit
//	--no-extensions             Disable all extensions
//	-e <path>                   Load extension (can be repeated)
//	--skill <name>              Load a skill
//	--cwd <dir>                 Working directory (default: current dir)
//	--agent-dir <dir>           Agent config directory (default: <config>/agent)
//	--version                   Print version and exit
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"
	"github.com/MichaelKinsy/PiG/coding/extension/host/cellpack"
	"github.com/MichaelKinsy/PiG/coding/extension/host/fusepack"
	"github.com/MichaelKinsy/PiG/coding/extension/host/inproc"
	"github.com/MichaelKinsy/PiG/coding/extension/host/subprocess"
	"github.com/MichaelKinsy/PiG/coding/extension/pigsdk"
	piglet "github.com/MichaelKinsy/PiG/coding/piglet"
	"github.com/MichaelKinsy/PiG/coding/pigletbuild"
	"github.com/MichaelKinsy/PiG/coding/pigletbuild/binarypiglet"
	"github.com/MichaelKinsy/PiG/internal/codingagent"
	"github.com/MichaelKinsy/PiG/internal/codingagent/export"
	"github.com/MichaelKinsy/PiG/internal/codingagent/prompts"
	"github.com/MichaelKinsy/PiG/internal/codingagent/tools"
	"github.com/MichaelKinsy/PiG/internal/nativeplatform"
	"github.com/MichaelKinsy/PiG/internal/pigdocs"
	"github.com/MichaelKinsy/PiG/internal/profiling"
	"github.com/MichaelKinsy/PiG/internal/resolvepath"
	"github.com/MichaelKinsy/PiG/tui"
)

// PigVersion is pig's own release line. UpstreamVersion is the pi tag this
// build targets. The literal lives in internal/coding/pigversion/pigversion.go;
// coding/upstream.go aliases it, so the parity runner (and this constant)
// can import it without depending on package main.
const (
	PigVersion      = coding.PigVersion
	UpstreamVersion = coding.UpstreamVersion
	Version         = coding.Version

	startupSDKLockTimeout = 2 * time.Second
)

// Build is the git commit/tag, set via -ldflags at build time.
var Build = "dev"

func buildIdentity() string {
	if Build != "dev" {
		return Build
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return Build
	}
	return resolveBuildIdentity(Build, info.Main.Version)
}

func resolveBuildIdentity(linkerBuild, moduleVersion string) string {
	if linkerBuild != "dev" {
		return linkerBuild
	}
	if moduleVersion == "" || moduleVersion == "(devel)" {
		return linkerBuild
	}
	return moduleVersion
}

// pig divergence (D39): PigletBinaryVersion is the baked release identity.
var PigletBinaryVersion string

// selfUpdateVersion uses the baked release version when present.
func selfUpdateVersion() string {
	if codingagent.PigletBinaryRelease != "" {
		return codingagent.PigletBinaryRelease
	}
	return PigVersion
}

// cliVersionString is the `pig --version` output.
func cliVersionString() string {
	// pig divergence (D63): --version prints PiG's composite version, not the bare Pi version.
	return Version
}

// versionString returns pig's self-identification string for help
// and diagnostics banners.
func versionString() string {
	return fmt.Sprintf("pig %s [%s %s/%s build=%s]",
		Version, runtime.Version(), runtime.GOOS, runtime.GOARCH, buildIdentity())
}

func detailedVersionString() string {
	return fmt.Sprintf("pig: %s\nupstream pi: %s\ngo: %s\nplatform: %s/%s\nbuild: %s",
		PigVersion, UpstreamVersion, runtime.Version(), runtime.GOOS, runtime.GOARCH, buildIdentity())
}

// ─── Extension Loading ────────────────────────────────────────────────────────

// ─── Resource Loading ────────────────────────────────────────────────────────────

// loadSkills loads ordered skill inputs, validates descriptions, and returns
// first-wins definitions with collision diagnostics for the interactive listing.
// A missing named skill remains an error; invalid discovered files are warned.
func loadSkills(skillInputs []string, noSkills bool) ([]*codingagent.SkillDef, []extension.ResourceDiagnostic, error) {
	if noSkills || len(skillInputs) == 0 {
		return nil, nil, nil
	}
	var skillDefs []*codingagent.SkillDef
	for _, input := range skillInputs {
		if input == "" {
			continue
		}
		if _, err := os.Stat(input); err == nil {
			loaded, err := codingagent.LoadSkills(codingagent.LoadSkillsOptions{AgentDir: codingagent.DefaultAgentDir(), SkillPaths: []string{input}})
			if err != nil {
				return skillDefs, nil, err
			}
			for _, diagnostic := range loaded.Diagnostics {
				fmt.Fprintf(os.Stderr, "warning: %s: %s\n", diagnostic.Path, diagnostic.Message)
			}
			skillDefs = append(skillDefs, loaded.Skills...)
			continue
		}
		def, err := codingagent.LoadSkill(codingagent.DefaultSkillsDir(), input)
		if err != nil {
			return skillDefs, nil, fmt.Errorf("--skill %q: %w", input, err)
		}
		for _, diagnostic := range codingagent.SkillDiagnostics(def) {
			fmt.Fprintf(os.Stderr, "warning: %s: %s\n", def.Path, diagnostic)
		}
		if strings.TrimSpace(def.Description) != "" {
			skillDefs = append(skillDefs, def)
		}
	}
	defs, diagnostics := codingagent.DeduplicateSkillsWithDiagnostics(skillDefs)
	return defs, diagnostics, nil
}

// ─── Initial Message ──────────────────────────────────────────────────────────

// readPipedStdin returns piped stdin content, trimmed. It returns "" when
// stdin is a terminal or the content is blank. Mirrors upstream main.ts
// readPipedStdin (`data.trim() || undefined`).
//
// The read waits for end of input, so a writer that never closes the pipe
// keeps it waiting, as upstream's does. A termination signal ends the wait:
// upstream reads stdin before print mode registers its signal handlers, so
// the signal's default action stops the process mid-read. It returns ctx's
// error when ctx ends first.
func readPipedStdin(ctx context.Context) (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return "", nil
	}
	read := make(chan string, 1)
	go func() {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			read <- ""
			return
		}
		read <- strings.TrimSpace(string(data))
	}()
	select {
	case content := <-read:
		return content, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// buildInitialMessage combines stdin content, @file text, and the first CLI
// message into the initial prompt, and returns the remaining CLI messages,
// which are sent one by one after it. Mirrors upstream cli/initial-message.ts:
// the parts are concatenated in order with no separator, and image files are
// returned as separate content blocks.
func buildInitialMessage(messages []string, fileText string, fileImages []ai.ImageContent, stdinContent string) (string, []ai.ImageContent, []string) {
	var parts []string
	if stdinContent != "" {
		parts = append(parts, stdinContent)
	}
	if fileText != "" {
		parts = append(parts, fileText)
	}
	if len(messages) > 0 {
		parts = append(parts, messages[0])
		messages = messages[1:]
	}
	var images []ai.ImageContent
	if len(fileImages) > 0 {
		images = fileImages
	}
	return strings.Join(parts, ""), images, slices.Clone(messages)
}

// prepareInitialMessage processes @file arguments without resizing and builds the initial prompt. Session selects the resize profile after extension hooks select the request model.
func prepareInitialMessage(cwd string, messages, fileArgs []string, stdinContent string) (string, []ai.ImageContent, []string, error) {
	if len(fileArgs) == 0 {
		initial, images, rest := buildInitialMessage(messages, "", nil, stdinContent)
		return initial, images, rest, nil
	}
	// upstream: packages/coding-agent/src/main.ts:prepareInitialMessage
	processed, err := codingagent.ProcessCLIFileArguments(fileArgs, cwd, codingagent.ProcessFileOptions{AutoResizeImages: new(false)})
	if err != nil {
		return "", nil, nil, err
	}
	initial, images, rest := buildInitialMessage(messages, processed.Text, processed.Images, stdinContent)
	return initial, images, rest, nil
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func runPigPreSessionCommand(args []string) int {
	if len(args) >= 2 && args[0] == "piglet" && args[1] == "build" {
		return pigletbuild.RunPigletBuildCommand(args[2:], os.Stdout, os.Stderr)
	}
	if len(args) >= 2 && args[0] == "piglet" && args[1] == "publish" {
		return pigletbuild.RunPigletPublishCommand(args[2:], os.Stdout, os.Stderr)
	}
	for _, run := range []func([]string, io.Writer, io.Writer) int{
		pigdocs.RunCommand,
		pigsdk.RunCommand,
		piglet.RunCommand,
	} {
		if code := run(args, os.Stdout, os.Stderr); code >= 0 {
			return code
		}
	}
	return -1
}

// enforcePigletBinaryScope fails closed when a Piglet Binary is asked to run a
// different Piglet than the one it was built with. A Piglet Binary is bound to
// one immutable composition; running another Piglet is out of scope, so it
// points the user at raw pig.
func enforcePigletBinaryScope(flags map[string]any) {
	if !binarypiglet.IsPigletBinary() {
		return
	}
	if pigletWasRequested(flags) {
		fmt.Fprintln(os.Stderr, "pig: this Piglet Binary runs its built-in Piglet; run a different Piglet with raw pig (pig --piglet <name|path>)")
		exitProcess(1)
	}
}

// resolvePiglet reads the piglet from --piglet flag or
// PIG_PIGLET_NAME/PIG_PIGLET_PATH env vars. Returns nil if no piglet.
// This is called early in startup: before model resolution, extension
// loading, and discovery: so that piglet fields can drive those steps.
func resolvePiglet(unknownFlags map[string]any) (*piglet.Piglet, bool) {
	// Find the piglet path from flag or env.
	var pigletPath string
	if v, ok := unknownFlags["piglet"]; ok {
		s, valid := v.(string)
		if !valid || strings.TrimSpace(s) == "" {
			fmt.Fprintln(os.Stderr, "warning: --piglet requires a name or path")
			return nil, false
		}
		pigletPath = s
	}

	// If it's a name (not a path), resolve it against the config roots.
	if pigletPath != "" {
		if _, err := os.Stat(pigletPath); err != nil {
			// Not a direct path: resolve as a name. Surface Resolve's
			// descriptive "not found in search paths" error instead of
			// falling through to Parse the bare name (which reported a
			// misleading "open <name>: no such file or directory").
			resolved, rerr := piglet.Resolve(pigletPath)
			if rerr != nil {
				fmt.Fprintf(os.Stderr, "warning: piglet %q: %v\n", pigletPath, rerr)
				return nil, false
			}
			pigletPath = resolved
		}
	}

	// Check env vars if no flag.
	if pigletPath == "" {
		if p := os.Getenv("PIG_PIGLET_PATH"); p != "" {
			pigletPath = p
		} else if name := os.Getenv("PIG_PIGLET_NAME"); name != "" {
			if resolved, err := piglet.Resolve(name); err == nil {
				pigletPath = resolved
			}
		}
	}

	if pigletPath == "" {
		// A Piglet Binary's baked Piglet has precedence over host/platform
		// defaults and cannot be replaced by an ambient /opt piglet.
		if p := binarypiglet.Default(); p != nil {
			return p, true
		}
		if _, err := os.Stat("/opt/pig/piglet.yaml"); err == nil {
			pigletPath = "/opt/pig/piglet.yaml"
		} else {
			return nil, false
		}
	}

	workspace, _ := os.Getwd()
	if value, ok := unknownFlags["workspace"]; ok {
		configured, valid := value.(string)
		if !valid || strings.TrimSpace(configured) == "" {
			fmt.Fprintln(os.Stderr, "warning: --workspace requires a directory")
			return nil, false
		}
		workspace = configured
	}
	resolution, err := piglet.ResolveEffectiveWithOptions(pigletPath, piglet.ResolveOptions{Workspace: workspace})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: piglet %s: %v\n", pigletPath, err)
		return nil, false
	}
	return resolution.Piglet, false
}

func inlinePigletSystemPrompt(p *piglet.Piglet) {
	if p == nil || p.SystemPrompt == nil || p.SystemPrompt.File == "" {
		return
	}
	promptPath := p.SystemPrompt.File
	if !filepath.IsAbs(promptPath) {
		promptPath = filepath.Join(filepath.Dir(p.SourcePath()), promptPath)
	}
	if data, err := os.ReadFile(promptPath); err == nil {
		p.SystemPrompt = &piglet.PromptRef{Text: string(data)}
	} else {
		fmt.Fprintf(os.Stderr, "warning: piglet %s: system prompt %s: %v\n", p.Name, p.SystemPrompt.File, err)
	}
}

// resolvePigletExtConfigs resolves a piglet's extension origins to
// subprocess.ExtConfig entries. Returns nil if no extensions with origins.
func resolvePigletExtConfigs(p *piglet.Piglet) []subprocess.ExtConfig {
	resolved, errs := piglet.ResolveExtensions(p)
	for _, err := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}

	if len(resolved) == 0 {
		return nil
	}

	var configs []subprocess.ExtConfig
	for _, r := range resolved {
		cfg, _, err := subprocess.ResolveExtConfigWithIdentity(r.Path, r.Entry.Name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: extension %s: %v\n", r.Path, err)
			continue
		}
		configs = append(configs, cfg)
	}
	return configs
}

func fusedConfigsForPiglet(p *piglet.Piglet) []subprocess.ExtConfig {
	if p == nil || len(p.Extensions) == 0 {
		return nil
	}
	wanted := make(map[string]struct{}, len(p.Extensions))
	for _, ext := range p.Extensions {
		wanted[ext.Name] = struct{}{}
	}
	var out []subprocess.ExtConfig
	for _, cfg := range fusepack.FusedConfigs() {
		if _, ok := wanted[cfg.Name]; ok {
			out = append(out, cfg)
		}
	}
	return out
}

func embeddedCellsFromCellpack(cells []cellpack.LoadedCell) []subprocess.EmbeddedCell {
	out := make([]subprocess.EmbeddedCell, 0, len(cells))
	for _, cell := range cells {
		exts := make([]subprocess.EmbeddedExtension, len(cell.Extensions))
		for i, ext := range cell.Extensions {
			exts[i] = subprocess.EmbeddedExtension{Name: ext.Name, Hash: ext.Hash}
		}
		out = append(out, subprocess.EmbeddedCell{Language: cell.Language, Key: cell.Key, Strategy: cell.Strategy, BinaryPath: cell.BinaryPath, Extensions: exts})
	}
	return out
}

// pigletModelSpec returns a model spec string from the piglet's model
// config, or "" if not set. The returned spec follows the standard
// "provider/name" convention and can be passed to resolveModel().
func pigletModelSpec(p *piglet.Piglet) string {
	if p == nil || p.Model == nil {
		return ""
	}
	name := p.Model.Name
	if name == "" {
		return ""
	}
	if p.Model.Provider != "" && !strings.Contains(name, "/") {
		return p.Model.Provider + "/" + name
	}
	return name
}

// applyPigletPreStart applies Piglet defaults needed before model/session setup.
func applyPigletPreStart(p *piglet.Piglet, flags *CLIFlags) {
	if p == nil {
		return
	}

	// Apply the piglet's system prompt as the base prompt (like
	// --system-prompt) unless the user passed one explicitly. resolvePiglet
	// inlines a file: reference to Text, so this is path-independent.
	if flags.SystemPrompt == "" && p.SystemPrompt != nil && p.SystemPrompt.Text != "" {
		flags.SystemPrompt = p.SystemPrompt.Text
	}
}

func pigletAmbientSources(p *piglet.Piglet, kind string) *[]string {
	if p == nil {
		return nil
	}
	empty := []string{}
	if p.Discovery == nil {
		return &empty
	}
	if kind == "extensions" {
		return &p.Discovery.Extensions
	}
	return &p.Discovery.Skills
}

func runRetiredCommand(args []string) int {
	if len(args) == 0 || args[0] != "resource" {
		return -1
	}
	fmt.Fprintln(os.Stderr, "pig: unknown command resource; use `pig config` to change Resource filters or `pig status --json` to inspect Resources")
	return 2
}

func stageExtensionSDKsAtStartup(ctx context.Context, configRoot string, stderr io.Writer, timeout time.Duration) {
	stageCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := pigsdk.EnsureSyncedContext(stageCtx, configRoot); err != nil {
		_, _ = fmt.Fprintf(stderr, "pig: warning: extension SDK staging failed: %v\n", err)
		_, _ = fmt.Fprintln(stderr, "pig: startup will continue; extensions may build against a stale SDK; run 'pig diagnose' for detail")
	}
}

func main() {
	// PIG_PROFILE (internal/profiling) is read once here. Unset, it costs one lookup.
	stopProfiles = profiling.Start()
	defer stopProfiles()
	defer nativeplatform.ShutdownClipboard()
	defer exitOnRenderOverflow()

	binaryPath := guardBinaryIdentity()
	setupCli()

	// pig divergence (D39): publish update ownership before command dispatch.
	codingagent.PigletBinaryRelease = PigletBinaryVersion
	codingagent.InstalledPigVersion = PigVersion

	// pig additive (D18): a Piglet Binary verifies its baked closure and
	// signature before any command or session behavior. Stock Pig has no baked
	// closure, so this is a no-op there.
	if err := binarypiglet.Verify(); err != nil {
		fmt.Fprintf(os.Stderr, "pig: Piglet Binary verification failed: %v\n", err)
		exitProcess(1)
	}

	// Subcommands run before a Piglet Binary initializes session extensions.
	if len(os.Args) > 1 {
		if code := runAuthCommand(os.Args[1:]); code >= 0 {
			exitProcess(code)
		}
	}
	// Upstream main clears an earlier npm self-update's quarantine on every
	// Windows start after the auth command.
	if runtime.GOOS == "windows" {
		codingagent.CleanupWindowsSelfUpdateQuarantine(codingagent.GetPackageDir())
	}
	if len(os.Args) > 1 {
		if code := runLoginCommand(os.Args[1:]); code >= 0 {
			exitProcess(code)
		}
		if code := runStatusCommand(os.Args[1:]); code >= 0 {
			exitProcess(code)
		}
		if code := runSetupCommand(os.Args[1:], os.Stdout, os.Stderr); code >= 0 {
			exitProcess(code)
		}
		if code := runVerifyCommand(os.Args[1:], os.Stdout, os.Stderr); code >= 0 {
			exitProcess(code)
		}
		if code := runPackageCommand(os.Args[1:]); code >= 0 {
			exitProcess(code)
		}
		if code := runBuildCommand(os.Args[1:], os.Stdout, os.Stderr); code >= 0 {
			exitProcess(code)
		}
		if code := runExtensionCommand(os.Args[1:]); code >= 0 {
			exitProcess(code)
		}
		if code := runExtensionsCommand(os.Args[1:]); code >= 0 {
			exitProcess(code)
		}
		if code := runRetiredCommand(os.Args[1:]); code >= 0 {
			exitProcess(code)
		}
		switch os.Args[1] {
		case "config":
			exitProcess(runConfigCommand(os.Args[2:]))
		case "diagnose":
			runDiagnose(os.Stdout, binaryPath)
			exitProcess(0)
		case "version":
			fmt.Println(detailedVersionString())
			exitProcess(0)
		}
		if code := runPigPreSessionCommand(os.Args[1:]); code >= 0 {
			exitProcess(code)
		}
	}

	// A Piglet Binary embeds prebuilt cells and may link fused extensions. Register
	// both only when starting a session; stock Pig carries empty registries.
	if err := cellpack.Register(); err != nil {
		fmt.Fprintf(os.Stderr, "pig: Piglet Binary cells: %v\n", err)
	}
	fusepack.Register()

	// pig additive (D18): verify the baked extension closure before startup.
	fusedNames := make([]string, 0)
	for _, cfg := range fusepack.FusedConfigs() {
		fusedNames = append(fusedNames, cfg.Name)
	}
	packedNames := make([]string, 0)
	for _, cell := range cellpack.LoadedCells() {
		for _, ext := range cell.Extensions {
			packedNames = append(packedNames, ext.Name)
		}
	}
	if err := binarypiglet.VerifyRegisteredClosure(fusedNames, packedNames); err != nil {
		fmt.Fprintf(os.Stderr, "pig: %v\n", err)
		exitProcess(1)
	}

	flags := parseFlags(os.Args[1:])
	trace.Mark("flags-parsed")
	if reportArgDiagnostics(os.Stderr, flags.Diagnostics, term.IsTerminal(int(os.Stderr.Fd()))) {
		exitProcess(1)
	}

	// Pi routes version output before Session validation (main.ts:619-622).
	if flags.Version {
		fmt.Println(cliVersionString())
		exitProcess(0)
	}

	// --session-id conflict check. Mirrors upstream main.ts:217-230 (v0.76.0).
	if flags.SessionID != "" {
		// Validate format. Mirrors upstream assertValidSessionId (session-manager.ts).
		if !isValidSessionID(flags.SessionID) {
			printCLIError("session id must be non-empty, contain only alphanumeric characters, '-', '_', and '.', and start and end with an alphanumeric character")
			exitProcess(1)
		}
		var conflicts []string
		if flags.Session != "" {
			conflicts = append(conflicts, "--session")
		}
		if flags.Continue {
			conflicts = append(conflicts, "--continue")
		}
		if flags.ResumeAny {
			conflicts = append(conflicts, "--resume")
		}
		if len(conflicts) > 0 {
			printCLIError("--session-id cannot be combined with %s", strings.Join(conflicts, ", "))
			exitProcess(1)
		}
	}

	exportOfflineMode(flags.Offline)

	// --list-models is handled after extensions load (below), so
	// extension-contributed providers appear in the catalog, matching upstream
	// which lists against the extension-populated modelRuntime.

	// --export: export session file to HTML and exit.
	// Mirrors upstream main.ts:459-466.
	if flags.Export != "" {
		outputPath := ""
		if len(flags.Args) > 0 {
			outputPath = flags.Args[0]
		}
		result, err := export.ExportFromFile(flags.Export, outputPath)
		if err != nil {
			printCLIError("%v", err)
			exitProcess(1)
		}
		fmt.Fprintf(os.Stderr, "Exported to: %s\n", result)
		exitProcess(0)
	}

	// Non-interactive startup reserves stdout except for plain runtime metadata (Pi main.ts:129-130,638-642).
	if processAppMode(flags) != appModeInteractive && !isPlainRuntimeMetadataCommand(flags) {
		if err := codingagent.TakeOverStdout(); err != nil {
			printCLIError("%v", err)
			exitProcess(1)
		}
	}

	// Upstream main.ts runs validateForkFlags after --version and --export.
	if reportArgDiagnostics(os.Stderr, validateForkFlags(flags), term.IsTerminal(int(os.Stderr.Fd()))) {
		exitProcess(1)
	}

	initialCWD, err := os.Getwd()
	if err != nil {
		printCLIError("get cwd: %v", err)
		exitProcess(1)
	}

	// Set up context with signal handling.
	//
	// SIGTERM and SIGHUP dispose the runtime before cancelling. Print/JSON
	// leave SIGINT to the default process action; interactive mode owns D51.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	if runtime.GOOS != "windows" {
		signal.Notify(sigCh, syscall.SIGHUP)
	}
	defer signal.Stop(sigCh)
	go func() {
		sig := <-sigCh
		if sysSig, ok := sig.(syscall.Signal); ok {
			receivedTerminationSignal.Store(int32(sysSig))
		}
		// Give extensions their cleanup event before cancelling: the root
		// context owns the extension subprocesses, so cancelling it kills the
		// very processes that would receive session_shutdown. Upstream's
		// signal-triggered shutdown likewise disposes the runtime first.
		runTerminationShutdownHook()
		cancel()
	}()

	// A required Piglet environment must own the whole coding-agent process,
	// so resolve and enter it before services, model, extensions, or tools start.
	var activePiglet *piglet.Piglet
	var activePigletBaked bool
	enforcePigletBinaryScope(flags.UnknownFlags)
	activePiglet, activePigletBaked = resolvePiglet(flags.UnknownFlags)
	if activePigletBaked && flags.SystemPrompt != "" {
		fmt.Fprintln(os.Stderr, "pig: a Piglet Binary's baseline system prompt cannot be replaced; use --append-system-prompt for an additive prompt")
		exitProcess(1)
	}
	if activePiglet == nil && pigletWasRequested(flags.UnknownFlags) {
		fmt.Fprintln(os.Stderr, "pig: the requested Piglet could not be loaded")
		exitProcess(1)
	}
	handled, exitCode, environmentErr := runPigletAgentEnvironment(ctx, activePiglet, flags, initialCWD)
	if environmentErr != nil {
		fmt.Fprintf(os.Stderr, "pig: %v\n", environmentErr)
		exitProcess(1)
	}
	if handled {
		exitProcess(exitCode)
	}
	inlinePigletSystemPrompt(activePiglet)
	if activePiglet != nil && (activePiglet.AgentEnv == nil || piglet.ActiveAgentEnvironmentIdentity() != "") {
		resolvedSecrets, err := piglet.ResolveRequiredSecrets(ctx, activePiglet)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pig: Piglet secrets: %v\n", err)
			exitProcess(1)
		}
		resolvedSecrets.Clear()
	}

	// pig does not use undici's default 300s body/header timeouts. Our
	// streaming HTTP clients are built on Go's net/http with no overall client
	// timeout and ResponseHeaderTimeout explicitly disabled for long-lived SSE
	// streams; provider-specific deadlines still come from context cancellation.

	// Resolve directories.
	// Working directory comes from os.Getwd() (no CLI override; matches upstream).
	cwd := initialCWD
	agentDir := codingagent.AgentDir()
	agentDirForModelOverride = agentDir

	// Run one-shot config migrations before loading services/resources so
	// renamed directories (e.g. commands/ → prompts/) are visible on the
	// current startup path. Mirrors upstream startup ordering.
	if _, _, err := codingagent.RunMigrations(cwd, agentDir); err != nil {
		printCLIError("%v", err)
		exitProcess(1)
	}

	// Bootstrap global proxy settings before constructing any provider clients; Session-local settings are resolved after Session selection.
	startupSettingsManager := codingagent.NewSettingsManager(cwd, agentDir)
	if err := ai.ApplyHTTPProxySettings(startupSettingsManager.GetGlobalSettings().HTTPProxy); err != nil {
		printCLIError("%v", err)
		exitProcess(1)
	}
	startupSettingsDiagnostics := codingagent.CollectSettingsDiagnostics(startupSettingsManager)
	sessionDir, err := resolveSessionDir(flags.SessionDir, startupSettingsManager)
	if err != nil {
		printCLIError("%v", err)
		exitProcess(1)
	}
	startupUIOpts := codingagent.StartupUIOptions{
		AgentDir:   agentDir,
		Settings:   startupSettingsManager.GetGlobalSettings(),
		ThemePaths: collectStartupThemePaths(initialCWD, agentDir, startupSettingsManager),
	}
	// Pi createSessionManager selects in-memory modes before considering the resume picker.
	if flags.ResumeAny && !flags.NoSession && !flags.Help && flags.ListModels == "" && !flags.ListModelsAll {
		if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
			exitProcess(0)
		}
		manager := newSessionManagerWithDir(initialCWD, sessionDir)
		selected, ok, selectErr := codingagent.SelectStartupSession(
			func(options codingagent.SessionListOptions) ([]codingagent.SessionInfo, error) {
				return manager.ListCurrentSessions(options)
			},
			func(options codingagent.SessionListOptions) ([]codingagent.SessionInfo, error) {
				return manager.ListAllSessions(options)
			},
			startupUIOpts,
		)
		if selectErr != nil {
			printCLIError("select session: %v", selectErr)
			exitProcess(1)
		}
		if !ok {
			// Upstream main.ts:422 console.log(chalk.dim(...)): faint only when
			// chalk's stdout color level is non-zero.
			msg := "No session selected"
			if chalkColorLevel(environMap(os.Environ()), os.Args[1:], true) > 0 {
				msg = "\x1b[2m" + msg + "\x1b[22m"
			}
			_, _ = fmt.Fprintln(os.Stdout, msg)
			exitProcess(0)
		}
		flags.ResumeAny = false
		flags.Session = selected
	}
	startupSession, err := resolveStartupSessionSelection(flags, initialCWD, sessionDir)
	if err != nil {
		_, missing := errors.AsType[*sessionNotFoundError](err)
		_, exists := errors.AsType[*sessionAlreadyExistsError](err)
		_, fileURL := errors.AsType[*sessionPathURLError](err)
		if missing || exists || fileURL {
			color := chalkColorLevel(environMap(os.Environ()), os.Args[1:], term.IsTerminal(int(os.Stdout.Fd()))) > 0
			fmt.Fprintln(os.Stderr, formatStartupSessionError(err, color))
		} else {
			printCLIError("%v", err)
		}
		exitProcess(1)
	}
	if crossProject := startupSession.crossProject; crossProject != nil {
		confirmed, confirmErr := confirmCrossProjectSession(os.Stdin, os.Stdout, crossProject.cwd)
		if confirmErr != nil {
			printCLIError("confirm cross-project session: %v", confirmErr)
			exitProcess(1)
		}
		if !confirmed {
			_, _ = fmt.Fprintln(os.Stdout, "Aborted.")
			exitProcess(0)
		}
		manager := newSessionManagerWithDir(startupSession.runtimeCWD, sessionDir)
		forked, forkErr := manager.ForkFromFile(crossProject.path)
		if forkErr != nil {
			printCLIError("%v", forkErr)
			exitProcess(1)
		}
		startupSession.forkPath = forked.Path()
		startupSession.crossProject = nil
	}
	if issue := startupSession.missingCWD; issue != nil {
		if processAppMode(flags) != appModeInteractive {
			fmt.Fprintf(os.Stderr, "%s\n", issue.Error())
			exitProcess(1)
		}
		selected, ok, selectErr := codingagent.ShowStartupSelector(
			issue.prompt(),
			[]string{"Continue", "Cancel"},
			startupUIOpts,
		)
		if selectErr != nil {
			printCLIError("select session cwd: %v", selectErr)
			exitProcess(1)
		}
		if !ok || selected != 0 {
			exitProcess(0)
		}
		startupSession.runtimeCWD = issue.fallbackCWD
		startupSession.missingCWD = nil
		flags.sessionCwdOverride = new(startupSession.runtimeCWD)
	}
	if flags.Continue && startupSession.resumePath == "" {
		manager := newSessionManagerWithDir(initialCWD, sessionDir)
		fmt.Fprintf(os.Stderr, "warning: no prior session in %s; starting fresh\n", manager.SessionDir())
	}
	cwd = startupSession.runtimeCWD
	sessionDir = startupSession.sessionDir
	// Session selection and its effects precede name normalization and validation (Pi main.ts:680-701).
	sessionName, nameErr := sessionNameFromFlags(flags)
	if nameErr != nil {
		printCLIError("%v", nameErr)
		exitProcess(1)
	}
	if applied, nameErr := startupSession.applyName(sessionName); nameErr != nil {
		printCLIError("set session name: %v", nameErr)
		exitProcess(1)
	} else if applied {
		sessionName = ""
	}
	flags.Name = sessionName
	resourceFlags, resolveErr := resolveCLIResourceFlags(flags, initialCWD)
	if resolveErr != nil {
		printCLIError("%v", resolveErr)
		exitProcess(1)
	}
	hasTrustResources := codingagent.HasTrustRequiringProjectResources(cwd)
	projectTrusted := flags.ProjectTrustOverride != nil && *flags.ProjectTrustOverride
	if flags.ProjectTrustOverride == nil && !hasTrustResources {
		projectTrusted = true
	}

	// Construct the SDK Services container. Mirrors what library
	// consumers of github.com/MichaelKinsy/PiG/coding will do;
	// the binary uses the same path so SDK + CLI share the same
	// startup semantics.
	traceSDKLockWaits()
	trace.Mark("pre-services")
	services, err := coding.NewServices(coding.ServicesOptions{
		CWD:            cwd,
		AgentDir:       agentDir,
		ProjectTrusted: new(projectTrusted),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "pig: services init: %v\n", err)
		exitProcess(1)
	}
	stopModelServices = services.Close
	defer services.Close()
	trace.Mark("services-created")
	llamaHost := startBuiltInLlama(ctx, services)
	settings := services.Settings()
	// Mirrors upstream main.ts: --use-theme and --tui-mode apply to this run only.
	if flags.UseTheme != "" {
		settings.Theme = flags.UseTheme
	}
	if flags.TuiMode != "" {
		settings.TuiMode = flags.TuiMode
	}

	trace.Mark("sdk-sync-start")
	// Stage the extension SDKs before the first extension load, including the
	// pre-trust load below, so out-of-tree `-e` builds and packed cells resolve
	// them on a clean host with no PiG checkout.
	//
	// Stock Pig always stages its extension SDKs. Skipping the stage would leave
	// extensions building against a stale copy on disk. A failure here is not
	// fatal, but it must be visible: its symptom is an extension carrying a bug
	// that was already fixed, which is otherwise untraceable from the running
	// process.
	stageExtensionSDKsAtStartup(ctx, codingagent.ConfigRoot(), os.Stderr, startupSDKLockTimeout)
	trace.Mark("sdk-sync-done")

	startupExtensions := &startupExtensionSet{}
	startupSourceResolver := newStartupExtensionSourceResolver(nil)
	stopStartupExtensions = startupExtensions.close
	defer startupExtensions.close()
	// Mirrors upstream loadFinalExtensionSet: pre-trust load errors are part of
	// the final extension errors.
	var preTrustExtensionDiagnostics []codingagent.AgentSessionRuntimeDiagnostic
	if flags.ProjectTrustOverride == nil && hasTrustResources {
		trace.Mark("trust-preload-start")
		userScope := []string{"user"}
		preTrustConfigs := collectExtensionConfigs(cwd, agentDir, services.SettingsManager(), resourceFlags, &userScope, startupSourceResolver.Resolve)
		preTrustExts, _, preTrustBridge, preTrustLoadErrs := loadFinalSubprocessExtensions(
			ctx,
			cwd,
			processAppMode(flags).extensionMode(),
			services.Registry().ModelRegistry,
			preTrustConfigs,
			nil,
			nil,
			startupExtensions,
		)
		trace.Mark("trust-preload-done")
		preTrustExtensionDiagnostics = extensionLoadDiagnostics(preTrustLoadErrs)
		var trustRunner *inproc.Runner
		if len(preTrustExts) > 0 {
			trustRunner = inproc.NewRunner(preTrustExts, cwd)
		}
		trustUI := extension.NoopUIContext
		interactiveTrust := processAppMode(flags) == appModeInteractive && !flags.Help && flags.ListModels == "" && !flags.ListModelsAll
		if interactiveTrust {
			trustUI = newStartupTrustUI(startupUIOpts)
			if preTrustBridge != nil {
				preTrustBridge.SetUIContext(trustUI)
			}
		}
		globalDefaultTrust := startupSettingsManager.GetGlobalSettings().DefaultProjectTrust
		if globalDefaultTrust == "" {
			globalDefaultTrust = "ask"
		}
		projectTrusted, err = resolveProjectTrusted(ctx, projectTrustResolutionOptions{
			CWD:     cwd,
			Store:   codingagent.NewProjectTrustStore(agentDir),
			Default: globalDefaultTrust,
			Runner:  trustRunner,
			UI:      trustUI,
			OnExtensionError: func(message string) {
				fmt.Fprintln(os.Stderr, "warning:", message)
			},
		})
		if err != nil {
			printCLIError("resolve project trust: %v", err)
			exitProcess(1)
		}
		services.SettingsManager().SetProjectTrusted(projectTrusted)
		settings = services.Settings()
		trace.Mark("trust-resolved")
	}
	// Mirrors upstream main.ts, which applies the terminal capability
	// overrides once the runtime settings exist and before any mode runs.
	tui.SetCapabilityOverrides(services.SettingsManager().GetTerminalCapabilityOverrides())
	// Mirrors upstream main.ts: startup and runtime settings managers can
	// report the same file error, so the combined list is deduplicated.
	startupDiagnostics := codingagent.DeduplicateDiagnostics(append(startupSettingsDiagnostics, codingagent.CollectSettingsDiagnostics(services.SettingsManager())...))

	// Wire --models CLI flag to settings.EnabledModels so the interactive
	// mode's scopedModelIDs picks them up. Mirrors upstream main.ts
	// resolveModelScope → session.scopedModels.
	if len(flags.Models) > 0 {
		settings.EnabledModels = flags.Models
	}

	// Reinstall any configured packages whose install directory is missing
	// on disk. Mirrors upstream onMissing callback flow in
	// DefaultPackageManager.resolve (package-manager.ts:839).
	trace.Mark("packages-reinstall-start")
	if reinstalled, missingPkgs := EnsureConfiguredPackagesInstalled(cwd, services.SettingsManager()); len(reinstalled)+len(missingPkgs) > 0 {
		if len(reinstalled) > 0 {
			fmt.Fprintf(os.Stderr, "[pig] Reinstalled missing packages: %s\n", strings.Join(reinstalled, ", "))
		}
		if len(missingPkgs) > 0 {
			fmt.Fprintf(os.Stderr, "[pig] Could not install: %s (run `pig update` when online)\n", strings.Join(missingPkgs, ", "))
		}
	}
	trace.Mark("packages-reinstall-done")

	promptPaths := collectPromptPaths(cwd, agentDir, services.SettingsManager(), resourceFlags, projectTrusted, startupSourceResolver.Resolve)
	themePaths := collectThemePaths(cwd, agentDir, services.SettingsManager(), resourceFlags, projectTrusted, startupSourceResolver.Resolve)
	extensionScopes := pigletAmbientSources(activePiglet, "extensions")
	skillScopes := pigletAmbientSources(activePiglet, "skills")
	if !projectTrusted {
		extensionScopes = trustedAmbientScopes(extensionScopes)
		skillScopes = trustedAmbientScopes(skillScopes)
	}
	if activePigletBaked {
		empty := []string{}
		extensionScopes = &empty
		skillScopes = &empty
	}
	if err := validateConfiguredPackagesForStartup(cwd, services.SettingsManager(), func(scope string) bool {
		return !resourceFlags.NoExtensions && packageScopeEnabled(extensionScopes, scope)
	}, startupSourceResolver.Resolve); err != nil {
		startupDiagnostics = append(startupDiagnostics, codingagent.AgentSessionRuntimeDiagnostic{Type: "warning", Message: err.Error()})
	}
	trace.Mark("packages-validated")
	skillInputs := collectSkillInputs(cwd, agentDir, services.SettingsManager(), resourceFlags, skillScopes, startupSourceResolver.Resolve)
	trace.Mark("extension-discovery-start")
	extraExtConfigs := collectExtensionConfigs(cwd, agentDir, services.SettingsManager(), resourceFlags, extensionScopes, startupSourceResolver.Resolve)
	trace.Mark("extension-discovery-done")

	// Apply the already-resolved Piglet before model and extension setup. The
	// environment gate above has either entered its runtime or explicitly
	// accepted the one-shot unsafe-host bypass.
	applyPigletPreStart(activePiglet, &flags)

	// Quiet startup banner so the user always knows which binary launched.
	// Suppressed in --print mode and when settings.QuietStartup is true.
	// --verbose overrides quietStartup (mirrors upstream args.ts:239).
	// pig divergence (D2): banner says "PiG", not "pi": separate binary
	// and config root avoid collisions with upstream pi.
	showBanner := flags.Print == "" && flags.Mode != "rpc" && (!settings.QuietStartup || flags.Verbose)
	var loginOperationalLines []string
	if showBanner {
		identityLine := fmt.Sprintf("PiG %s • config=%s • bin=%s",
			Version, codingagent.ConfigRoot(), binaryPath)
		startupHints := codingagent.StartupKeybindHints(codingagent.NewKeybindingsManager(agentDir))
		loginOperationalLines = append(loginOperationalLines, identityLine, "", startupHints)
	}

	trace.Mark("services-init")
	_ = pigdocs.EnsureSynced(codingagent.ConfigRoot())
	registry := services.Registry()

	// Load extensions before resolving the model, matching upstream main.ts:
	// createAgentSessionServices builds the extension-populated modelRuntime, and
	// resolveModelScope / listModels then run against it. Extension-contributed
	// model providers must be registered here so resolveModel and --list-models
	// see them. Also early so --print mode has extension tools.
	//
	// Piglet-driven extension loading: if a piglet resolves extension origins
	// to concrete paths, it becomes the sole authority for what loads and
	// convention-directory scanning is suppressed. A scoping-only piglet
	// resolves nothing and leaves -e/convention loading intact (see below).
	if activePiglet != nil {
		pigletExtConfigs := resolvePigletExtConfigs(activePiglet)
		// An embedded Piglet is authoritative even though its extension
		// origins were localized at build time. Embedded cells and fused configs
		// supply the runnable extensions; convention discovery must stay off so
		// local state cannot change the Piglet Binary.
		if activePigletBaked && len(activePiglet.Extensions) > 0 {
			flags.NoExtensions = true
			extraExtConfigs = mergeExtConfigs(append(fusedConfigsForPiglet(activePiglet), extraExtConfigs...))
		} else if len(pigletExtConfigs) > 0 {
			// A piglet drives extension loading only when it actually resolves
			// extensions to load (entries with an origin). A scoping-only piglet
			// (entries with tools but no origin, e.g. agent-server's "load via -e,
			// scope by name" pattern) resolves nothing; it must not suppress the
			// -e/convention loading, or the extensions it means to scope never
			// load. ScopeTools then filters the loaded set by name.
			flags.NoExtensions = true
			extraExtConfigs = mergeExtConfigs(append(pigletExtConfigs, extraExtConfigs...))
		}
	}

	// Every mode, RPC included, loads its final extension set here once, as upstream createAgentSessionServices does before model resolution; runRpcMode receives that runtime.
	var subprocExts []extension.Extension
	var subprocHost *subprocess.Host
	var subprocBridge *subprocess.UIBridge
	var embeddedCells []subprocess.EmbeddedCell
	if activePigletBaked {
		embeddedCells = embeddedCellsFromCellpack(cellpack.LoadedCells())
	}
	reloadExtensionConfigs := func() []subprocess.ExtConfig {
		configs := collectExtensionConfigs(cwd, agentDir, services.SettingsManager(), resourceFlags, extensionScopes)
		if activePiglet != nil {
			configs = append(resolvePigletExtConfigs(activePiglet), configs...)
		}
		return mergeExtConfigs(configs)
	}
	finalExtConfigs, reloadFinalExtConfigs := extraExtConfigs, reloadExtensionConfigs
	var rpcSourceInfo map[string]codingagent.ResourceSourceInfo
	if flags.Mode == "rpc" {
		rpcSourceInfo = resourceSourceInfoProvider(cwd, agentDir, services.SettingsManager(), resourceFlags, startupSourceResolver.Resolve)()
		finalExtConfigs, reloadFinalExtConfigs = rpcExtensionConfigs(extraExtConfigs, cwd, agentDir, rpcSourceInfo), nil
	}
	var extensionLoadErrs []error
	if !flags.NoExtensions || len(finalExtConfigs) > 0 || len(embeddedCells) > 0 {
		subprocExts, subprocHost, subprocBridge, extensionLoadErrs = loadFinalSubprocessExtensions(ctx, cwd, processAppMode(flags).extensionMode(), registry.ModelRegistry, finalExtConfigs, embeddedCells, reloadFinalExtConfigs, startupExtensions)
	}

	// In-process builtins are upstream's inline-factory tier: they follow path
	// extensions for dispatch and conflict ownership, and /reload invokes their
	// factories again rather than retaining closure state.
	var reloadBuiltinExtensions func() []extension.Extension
	if activePiglet != nil {
		reloadBuiltinExtensions = func() []extension.Extension {
			return []extension.Extension{piglet.BuildExtensionWithPiglet(activePiglet)}
		}
	}
	var builtinExts []extension.Extension
	if reloadBuiltinExtensions != nil {
		builtinExts = reloadBuiltinExtensions()
	}
	allExts := codingagent.ExtensionsInLoadOrder(subprocExts, builtinExts)
	trace.Mark("extensions-loaded")

	extensionDiagnostics := slices.Concat(preTrustExtensionDiagnostics, extensionLoadDiagnostics(extensionLoadErrs), extensionConflictDiagnostics(codingagent.DetectExtensionConflicts(allExts)))
	startupDiagnostics = append(startupDiagnostics, extensionDiagnostics...)

	// Resolve model
	// Piglet model acts as a fallback: --model flag > piglet.model > settings.defaultModel
	trace.Mark("pre-model")
	modelFlag := flags.Model
	if modelFlag == "" {
		modelFlag = pigletModelSpec(activePiglet)
	}
	if err := startupSession.loadSession(flags); err != nil {
		printCLIError("open selected session: %v", err)
		exitProcess(1)
	}
	selected, modelErr := selectStartupModel(ctx, startupModelOptions{
		SessionManager: startupSession.manager,
		CLIProvider:    flags.Provider,
		CLIModel:       modelFlag,
		CLIThinking:    flags.Thinking,
		ScopePatterns:  settings.EnabledModels,
		Continuing:     startupSession.resumePath != "" || startupSession.forkPath != "",
		APIKey:         flags.APIKey,
	}, settings, services)
	// resolveModelScope reports immediately; buildSessionOptions diagnostics follow extension errors in the runtime's report.
	for _, warning := range selected.ScopeWarnings {
		printModelDiagnostic(warning)
	}
	for _, warning := range selected.Warnings {
		startupDiagnostics = append(startupDiagnostics, codingagent.AgentSessionRuntimeDiagnostic{Type: "warning", Message: warning})
	}
	for _, diagnosticErr := range selected.Errors {
		startupDiagnostics = append(startupDiagnostics, codingagent.AgentSessionRuntimeDiagnostic{Type: "error", Message: diagnosticErr.Error()})
	}
	startupDiagnostics = codingagent.DeduplicateDiagnostics(startupDiagnostics)

	// Metadata includes scope warnings and startup settings diagnostics, but suppresses runtime diagnostics and their failure status.
	if flags.Help {
		codingagent.ReportDiagnostics(startupSettingsDiagnostics)
		printHelp(os.Stdout, term.IsTerminal(int(os.Stdout.Fd())), extensionHelpFlags(allExts)...)
		exitProcess(0)
	}
	if flags.ListModels != "" || flags.ListModelsAll {
		codingagent.ReportDiagnostics(startupSettingsDiagnostics)
		printModelList(registry.ModelRegistry, agentDir, flags.ListModels)
		exitProcess(0)
	}
	// RPC owns stdin as JSONL. Other modes read piped input before runtime diagnostics, and metadata commands above never consume it.
	var stdinContent string
	if flags.Mode != "rpc" {
		trace.Mark("stdin-read-start")
		stdinContent, err = readPipedStdin(ctx)
		if err != nil {
			if sig := receivedTerminationSignal.Load(); sig != 0 {
				exitProcess(128 + int(sig))
			}
			printCLIError("read stdin: %v", err)
			exitProcess(1)
		}
		trace.Mark("stdin-read-done")
	}
	initialMessage, initialImages, extraMessages, messageErr := prepareInitialMessage(initialCWD, flags.Args, flags.FileArgs, stdinContent)
	if messageErr != nil {
		printCLIError("%v", messageErr)
		exitProcess(1)
	}
	if len(extensionDiagnostics) > 0 || modelErr != nil {
		if len(extensionDiagnostics) > 0 {
			reportExtensionLoadFailures(startupDiagnostics)
		} else {
			codingagent.ReportDiagnostics(startupDiagnostics)
		}
		exitProcess(1)
	}
	model, specThinking := selected.Model, selected.Thinking
	// Pi's Agent substitutes its unknown model sentinel when selection finds none; modes still start and validate credentials only when prompting.
	// sdk.ts's modelFallbackMessage is interactive-only; general CLI/RPC diagnostics stay separate.
	noModelWarning := selected.ModelFallbackMessage
	if model == nil && noModelWarning == "" {
		noModelWarning = codingagent.FormatNoModelsAvailableMessage()
	}
	// Thinking override cascade: --thinking flag > model-spec ":medium" > piglet.model.thinking
	if specThinking != "" && flags.Thinking == "" {
		flags.Thinking = specThinking
	}
	if flags.Thinking == "" && activePiglet != nil && activePiglet.Model != nil && activePiglet.Model.Thinking != "" {
		flags.Thinking = activePiglet.Model.Thinking
	}

	trace.Mark("model-resolved")

	// Load agent definition (if --agent) and skills (if --skill).

	// Piglet-driven skill loading: if piglet skills are declared, use those
	// instead of convention-directory discovery. An embedded Piglet may carry
	// inline skill content so it does not depend on local ~/.pig/skills state.
	// resolveAndLoadSkills encapsulates the resolve+load wiring (the site of a
	// past bug where NoSkills was passed as loadSkills's kill-switch).
	slr, err := resolveAndLoadSkills(activePiglet, skillInputs)
	if err != nil {
		printCLIError("%v", err)
		exitProcess(1)
	}
	skillCatalog := codingagent.SlashCommandCatalog{CWD: cwd, AgentDir: agentDir, SourceInfo: resourceSourceInfoProvider(cwd, agentDir, services.SettingsManager(), resourceFlags, startupSourceResolver.Resolve)()}
	skillDefs := skillCatalog.WithSkillSources(slr.Defs)
	skillInputs = slr.Paths

	// Build the default system prompt. The tool list and hints
	// reflect the actual tools we'll hand to the agent loop.
	// registryToolNames is the full set of built-in tools that exist and can
	// be activated via --tools. agentToolNames is the ACTIVE set advertised in
	// the system prompt; it defaults to the upstream default-active set
	// (read/bash/edit/write), with powershell/grep/find/ls registered but
	// opt-in. Mirrors upstream allToolNames, defaultActiveToolNames
	// (sdk.ts:244), and system-prompt.ts:90.
	registryToolNames := tools.BuiltinToolNames()
	agentToolNames := []string{"read", "bash", "edit", "write"}
	// The defaultTools setting replaces the default active set, as upstream
	// sdk.ts configuredDefaultToolNames does.
	if settings.DefaultTools != nil {
		agentToolNames = slices.Clone(settings.DefaultTools)
	}
	toolHints := prompts.DefaultToolSnippets()
	// Collect per-tool prompt guidelines from tool schemas.
	// Mirrors upstream agent-session.ts:2273-2276.
	toolGuidelines := tools.DefaultToolGuidelines()
	allowed := map[string]struct{}(nil)
	// activeBuiltin, when non-nil, restricts which built-in tools are active
	// without gating extension tools (AllowedTools gates everything). nil =
	// all built-in tools. Used to apply the default-active set.
	var activeBuiltin map[string]struct{}
	skipBuiltinTools := flags.NoBuiltinTools
	if flags.NoBuiltinTools {
		agentToolNames = []string{}
	}
	// --no-tools disables all tools (empty allowlist).
	// --tools <list> restricts to those names. Mirrors upstream args.ts:97-104.
	switch {
	case flags.NoTools:
		allowed = make(map[string]struct{}) // empty = block all
		agentToolNames = []string{}
		skipBuiltinTools = false
	case len(flags.Tools) > 0:
		allowed = make(map[string]struct{}, len(flags.Tools))
		for _, t := range flags.Tools {
			allowed[t] = struct{}{}
		}
		if !flags.NoBuiltinTools {
			// Advertise the requested built-in tools in registry order.
			// AllowedTools does the real activation gating.
			filtered := make([]string, 0, len(flags.Tools))
			for _, n := range registryToolNames {
				if _, ok := allowed[n]; ok {
					filtered = append(filtered, n)
				}
			}
			agentToolNames = filtered
		}
	default:
		if !flags.NoBuiltinTools {
			// Default active set excludes grep/find/ls (upstream sdk.ts:244).
			// AllowedTools stays nil so extension tools remain active.
			activeBuiltin = make(map[string]struct{}, len(agentToolNames))
			for _, t := range agentToolNames {
				activeBuiltin[t] = struct{}{}
			}
		}
	}
	// --exclude-tools / -xt: deny specific tools. The excluded set is both
	// removed from the system-prompt advertisement (agentToolNames) and
	// applied as a denylist at the agent loop (excludedTools), so an excluded
	// tool is non-callable, not merely hidden. Gates built-in AND extension
	// tools. Mirrors upstream excludedToolNames (sdk.ts:246) + isAllowedTool
	// (agent-session.ts:2288).
	var excludedTools map[string]struct{}
	if len(flags.ExcludeTools) > 0 {
		excludedTools = make(map[string]struct{}, len(flags.ExcludeTools))
		for _, t := range flags.ExcludeTools {
			excludedTools[t] = struct{}{}
		}
		filtered := agentToolNames[:0]
		for _, n := range agentToolNames {
			if _, ok := excludedTools[n]; !ok {
				filtered = append(filtered, n)
			}
		}
		agentToolNames = filtered
	}
	promptSkills := make([]prompts.Skill, 0, len(skillDefs))
	for _, s := range skillDefs {
		promptSkills = append(promptSkills, prompts.Skill{Name: s.Name, Description: s.Description, Path: s.Path, DisableModelInvocation: s.DisableModelInvocation})
	}
	trace.Mark("pre-system-prompt")
	projectCtxFiles := loadContextFiles(cwd, agentDir, flags.NoContextFiles)
	promptCtxFiles := toPromptContextFiles(projectCtxFiles)
	resolvedPrompts := resolvePromptInputs(cwd, agentDir, flags, projectTrusted)
	promptOptions := prompts.Options{
		Cwd:            cwd,
		Tools:          agentToolNames,
		ToolHints:      toolHints,
		ToolGuidelines: toolGuidelines,
		Skills:         promptSkills,
		PigDocsPath:    filepath.Join(codingagent.ConfigRoot(), "docs"),
		AppendMode:     "append",
		ContextFiles:   promptCtxFiles,
	}

	if resolvedPrompts.custom != "" {
		promptOptions.CustomPrompt = resolvedPrompts.custom
		promptOptions.AppendMode = "replace"
	}
	promptOptions.AppendSystemPrompt = resolvedPrompts.append

	joinedAppend := resolvedPrompts.append

	systemPromptSections := prompts.BuildSystemPromptSections(promptOptions)
	systemPrompt := prompts.BuildDefaultPrompt(promptOptions)

	// Build the structured BuildSystemPromptOptions for extensions
	// receiving the before_agent_start event. Mirrors upstream
	// agent-session.ts:_rebuildSystemPrompt which constructs the
	// same options and exposes them on every before_agent_start.
	extContextFiles := make([]extension.SystemPromptContextFile, 0, len(promptCtxFiles))
	for _, cf := range promptCtxFiles {
		extContextFiles = append(extContextFiles, extension.SystemPromptContextFile{
			Path:    cf.Path,
			Content: cf.Content,
		})
	}
	extSkills := extensionPromptSkills(skillDefs)
	systemPromptOptions := extension.BuildSystemPromptOptions{
		CustomPrompt:       resolvedPrompts.custom,
		CustomPromptSet:    resolvedPrompts.customSet,
		SelectedTools:      append([]string{}, agentToolNames...),
		ToolSnippets:       toolHints,
		ToolGuidelines:     toolGuidelines,
		AppendSystemPrompt: joinedAppend,
		Cwd:                cwd,
		ContextFiles:       extContextFiles,
		Skills:             extSkills,
	}

	// No agent-defined beforeToolCall hooks: agent persona system is
	// a pig-extension concern, not built into core.
	var beforeToolCall []agent.BeforeToolCallHook

	// RPC mode takes over protocol stdin/stdout with the already-loaded runtime.
	if flags.Mode == "rpc" {
		promptResult := codingagent.LoadPromptTemplates("", "", promptPaths...)
		promptTemplates := promptResult.Templates
		resumePath := startupSession.resumePath
		if startupSession.forkPath != "" {
			resumePath = startupSession.forkPath
		}
		rpcResources := rpcModeResources{
			PromptTemplates: promptTemplates,
			Skills:          rpcResolvedSkills(skillDefs, activePiglet),
			SourceInfo:      rpcSourceInfo,
			Extensions:      subprocExts,
			ExtensionHost:   subprocHost,
			ExtensionBridge: subprocBridge,
			Model:           model,
			ProjectTrusted:  projectTrusted,
			ResumePath:      resumePath,
			SessionManager:  startupSession.manager,
			Services:        services,
		}
		codingagent.ReportDiagnostics(startupDiagnostics)
		exitProcess(runRPCMode(ctx, flags, activePiglet, rpcResources))
	}

	// Startup session selection already ran before cwd-bound services. Headless
	// modes reuse the selected file; the interactive path below does the same.
	printResumePath := startupSession.resumePath
	if startupSession.forkPath != "" {
		printResumePath = startupSession.forkPath
	}

	// Print mode
	// Mirrors upstream main.ts resolveAppMode: --print, --mode json, or a
	// stdin or stdout that is not a terminal runs print mode.
	if processAppMode(flags) != appModeInteractive {
		// Print and JSON mode expand prompt templates as upstream
		// AgentSession.prompt does, so they load them as RPC mode does.
		promptResult := codingagent.LoadPromptTemplates("", "", promptPaths...)
		codingagent.ReportDiagnostics(startupDiagnostics)
		if subprocHost != nil {
			defer subprocHost.Shutdown("quit")
		}
		printExts := allExts
		// The print/JSON prompt comes from positional args, @files, and stdin.
		// --print is a bare boolean (upstream semantics); the prompt is in
		// initialMessage, which may be empty (upstream then sends nothing).
		mode := "text"
		if processAppMode(flags) == appModeJSON {
			mode = "json"
		}
		registryAllowed, registryExcluded := toolRegistryFilters(flags)
		host := printModeRuntime{
			UnknownFlags: flags.UnknownFlags,
			Commands: headlessCommandCatalog{
				promptTemplates: promptResult.Templates,
				skills:          rpcResolvedSkills(skillDefs, activePiglet),
				cwd:             cwd,
				agentDir:        agentDir,
				sourceInfo:      skillCatalog.SourceInfo,
				llama:           llamaHost,
			},
			ToolRegistryAllowed:  registryAllowed,
			ToolRegistryExcluded: registryExcluded,
			Services:             services,
			Extensions:           printExts,
			Bridge:               subprocBridge,
			Session: coding.SessionStartOptions{
				SessionManager:        startupSession.manager,
				ScopedModels:          extensionScopedModels(services, settings.EnabledModels),
				Model:                 model,
				ThinkingLevel:         ai.ThinkingLevel(flags.Thinking),
				SystemPrompt:          systemPrompt,
				SystemPromptSections:  systemPromptSections,
				SystemPromptResources: sessionPromptResources(resolvedPrompts, projectCtxFiles, skillDefs),
				AllowedTools:          allowed,
				ActiveBuiltinTools:    activeBuiltin,
				ExcludedTools:         excludedTools,
				SkipBuiltinTools:      skipBuiltinTools,
				BeforeToolCall:        beforeToolCall,
				NoSession:             flags.NoSession,
				SessionID:             flags.SessionID,
				SessionDir:            sessionDir,
				CWDOverride:           flags.sessionCwdOverride,
			},
			ResumePath:  printResumePath,
			SessionName: sessionName,
		}
		host.SystemPromptSections = func(skills []*codingagent.SkillDef) ai.OrderedSections {
			options := promptOptions
			options.Skills = promptSkillsFor(skills)
			return prompts.BuildSystemPromptSections(options)
		}
		host.SystemPromptResources = func(skills []*codingagent.SkillDef) *coding.SystemPromptResources {
			return sessionPromptResources(resolvedPrompts, projectCtxFiles, skills)
		}
		if err := runPrintMode(ctx, host, printModeOptions{Mode: mode, Messages: extraMessages, InitialMessage: initialMessage, InitialImages: initialImages}); err != nil {
			// A run stopped by a termination signal reports 128+signum and
			// stays quiet, matching upstream's print-mode signal handlers.
			if signalErr, ok := errors.AsType[*signalExitError](err); ok {
				exitProcess(signalErr.ExitCode())
			}
			if !errors.Is(err, errPrintModeHandled) {
				printCLIError("%v", err)
			}
			exitProcess(1)
		}
		return
	}

	// Interactive mode reuses the startup-selected Session. Bare --resume still
	// defers to the in-TUI picker until the shared U4 startup selector lands.
	resumePath := startupSession.resumePath
	forkPath := startupSession.forkPath

	// Build the SDK Runtime: same path any library consumer takes.
	// Runtime owns the extension runner + context and produces
	// *coding.Session values via New / Resume / Open / Continue.
	// subprocBridge is wired into InteractiveMode via SubprocessUIBridge.
	trace.Mark("pre-runtime")

	rt, err := coding.NewRuntime(coding.RuntimeOptions{
		Services:      services,
		NewExtensions: allExts,
		AbortContext:  ctx,
	})
	if err != nil {
		printCLIError("construct runtime: %v", err)
		exitProcess(1)
	}
	defer func() { _ = rt.Close() }()
	// Upstream /reload rediscovers extensions even when none loaded at
	// startup, so interactive mode keeps a reload-capable host.
	subprocHost, subprocBridge = ensureReloadableExtensionHost(subprocHost, subprocBridge, flags.NoExtensions, cwd, registry.ModelRegistry, reloadExtensionConfigs)
	if subprocHost != nil {
		defer subprocHost.Shutdown("quit")
	}

	settingsManager := services.SettingsManager()
	if err := configureHTTPDispatcherFromSettings(settingsManager); err != nil {
		printCLIError("configure HTTP dispatcher: %v", err)
		exitProcess(1)
	}

	// Create or reopen the Session with the resolved CLI thinking override before binding interactive presentation.
	startOpts := startupSession.startOptions(flags)
	startOpts.ScopedModels = extensionScopedModels(services, settings.EnabledModels)
	startOpts.Model = model
	startOpts.ThinkingLevel = ai.ThinkingLevel(flags.Thinking)
	startOpts.SystemPrompt = systemPrompt
	startOpts.SystemPromptSections = systemPromptSections
	startOpts.SystemPromptResources = sessionPromptResources(resolvedPrompts, projectCtxFiles, skillDefs)
	startOpts.AllowedTools = allowed
	startOpts.ActiveBuiltinTools = activeBuiltin
	startOpts.ExcludedTools = excludedTools
	startOpts.SkipBuiltinTools = skipBuiltinTools
	startOpts.BeforeToolCall = beforeToolCall
	trace.Mark("pre-session")
	var codingSess *coding.Session
	switch {
	case forkPath != "":
		// forkPath is the NEW forked file created above (its header records
		// parentSession). Open it to continue the fork.
		codingSess, err = rt.Open(forkPath, startOpts)
	case resumePath != "":
		codingSess, err = rt.Open(resumePath, startOpts)
	default:
		codingSess, err = rt.New(startOpts)
	}
	if err != nil {
		printCLIError("construct session: %v", err)
		exitProcess(1)
	}
	trace.Mark("session-created")
	defer func() { _ = codingSess.Close() }()
	// Initial metadata does not emit a runtime session_info_changed notification.
	if sessionName != "" {
		if _, err := codingSess.Inner().AppendSessionInfo(sessionName); err != nil {
			printCLIError("set session name: %v", err)
			exitProcess(1)
		}
	}
	displayResumePath := resumePath
	if forkPath != "" {
		displayResumePath = forkPath
	}
	requestAuthRuntime, err := codingagent.NewRequestAuthRuntime(ctx, codingagent.RequestAuthRuntimeOptions{
		Credentials: services.Auth(),
		AgentDir:    agentDir,
	})
	if err != nil {
		printCLIError("construct request auth runtime: %v", err)
		exitProcess(1)
	}
	interactiveRegistryAllowed, _ := toolRegistryFilters(flags)
	iopts := codingagent.InteractiveOptions{
		ContextUsage: func() (*int, int) {
			usage := codingSess.ContextUsage()
			if usage == nil {
				return nil, 0
			}
			return usage.Tokens, usage.ContextWindow
		},
		CWD:                 cwd,
		AgentDir:            agentDir,
		SessionDir:          sessionDir,
		Model:               model,
		NoModelWarning:      noModelWarning,
		StartupDiagnostics:  startupDiagnostics,
		Settings:            settings,
		SettingsManager:     settingsManager,
		SystemPrompt:        systemPrompt,
		SystemPromptOptions: systemPromptOptions,
		AllowedTools:        allowed,
		ToolRegistryAllowed: interactiveRegistryAllowed,
		ActiveBuiltinTools:  activeBuiltin,
		ExcludedTools:       excludedTools,
		NoBuiltinTools:      skipBuiltinTools,
		PromptPaths:         promptPaths,
		ThemePaths:          themePaths,
		NoPromptTemplates:   flags.NoPromptTemplates,
		NoThemes:            flags.NoThemes,
		UnknownFlags:        flags.UnknownFlags,
		BeforeToolCall:      beforeToolCall,
		InitialMessage:      initialMessage,
		InitialImages:       initialImages,
		InitialMessages:     extraMessages,
		AppVersion:          UpstreamVersion,
		PackageUpdateChecker: func() []string {
			updates := CheckForAvailableUpdates(cwd, services.SettingsManager())
			names := make([]string, 0, len(updates))
			for _, u := range updates {
				names = append(names, u.DisplayName)
			}
			return names
		},
		// pig divergence (D39): standalone-binary self-update notice at startup.
		BinaryUpdateChecker: func() *codingagent.BinaryUpdate {
			if IsOfflineModeEnabled() {
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			return codingagent.CheckForBinaryUpdate(ctx, &http.Client{Timeout: 6 * time.Second}, selfUpdateVersion())
		},
		ResourceSourceInfoProvider: resourceSourceInfoProvider(cwd, agentDir, services.SettingsManager(), flags, startupSourceResolver.Resolve),
		ReloadResourceProvider:     reloadResourceSnapshotProvider(cwd, agentDir, services.SettingsManager(), resourceFlags, skillScopes),
		Verbose:                    flags.Verbose,
		Skills:                     skillDefs,
		SkillDiagnostics:           slr.Diagnostics,
		RebuildSystemPrompt:        systemPromptRebuilder(cwd, agentDir, projectTrusted, flags, agentToolNames),
		BridgeExtensionTools:       coding.BridgeNewRunnerTools,
		SkillPaths:                 skillInputs,
		NoSkills:                   flags.NoSkills,
		ContextFiles:               projectCtxFiles,
		SystemPromptSourcePaths:    resolvedPrompts.sourcePaths,
		SessionHandle:              codingSess,
		ResumePath:                 displayResumePath,
		ModelBuilder: func(spec string) (*ai.Model, error) {
			return coding.BuildModel(spec, services)
		},
		DefaultModelPerProvider: codingagent.DefaultModelPerProvider(),
		ModelLookup:             codingSess.ModelRuntime().GetModel,
		ModelCatalog:            codingSess.ModelRuntime().GetModels,
		RequestAuthRuntime:      requestAuthRuntime,
		ModelRegistry:           services.Registry().ModelRegistry,
		ExtensionRunner:         rt.NewExtensionRunner(),
		ExtensionContext:        rt.ExtensionContext(),
		BuiltinExtensions:       builtinExts,
		ReloadBuiltinExtensions: reloadBuiltinExtensions,
		Llama:                   llamaHost,
		OfflineMode:             IsOfflineModeEnabled(),
		StartupMark:             trace.Mark,
		LoginHeaderOptions: codingagent.LoginHeaderOptions{
			OperationalLines: loginOperationalLines,
			TrueColor:        tui.SupportsTrueColor(),
		},
		LoginVisible: showBanner,
	}
	// pig-specific: wire subprocess extensions after struct init.
	// Must check concrete pointer before assigning to interface fields
	// to avoid typed-nil interface trap: a nil *subprocess.UIBridge
	// assigned to SubprocessUIBridge interface is non-nil (Go semantics),
	// causing panic on SetInvalidate call.
	if subprocBridge != nil {
		iopts.SubprocessUIBridge = subprocBridge
	}
	if subprocHost != nil {
		iopts.SubprocessHost = subprocHost
		// /reload recompiles these, and it is the command reached for after
		// rebuilding pig, so stage the embedded SDKs first for the same reason
		// startup does.
		iopts.StageExtensionSDKs = func() error {
			return pigsdk.EnsureSynced(codingagent.ConfigRoot())
		}
	}
	if flags.UseTheme != "" {
		iopts.InitialThemeSetting = &flags.UseTheme
	}
	interactive := codingagent.NewInteractiveMode(iopts)
	// SIGTERM must reach extensions before the root context is cancelled.
	setTerminationShutdownHook(interactive.ShutdownFromSignal)
	defer setTerminationShutdownHook(nil)

	trace.Mark("pre-interactive")
	if err := interactive.Run(ctx); err != nil {
		if errors.Is(err, codingagent.ErrInteractiveCrashed) {
			exitProcess(1)
		}
		printCLIError("%v", err)
		exitProcess(1)
	}
}

// toPromptContextFiles converts codingagent.ContextFile to the shape
// expected by prompts.Options.ContextFiles.
func toPromptContextFiles(cfs []codingagent.ContextFile) []struct{ Path, Content string } {
	out := make([]struct{ Path, Content string }, len(cfs))
	for i, cf := range cfs {
		out[i] = struct{ Path, Content string }{Path: cf.Path, Content: cf.Content}
	}
	return out
}

// resolveCLIResourceFlags resolves local -e, --skill, --prompt-template and
// --theme paths as main.ts:548-550 does: resolvePath(value, cwd) with no trim.
// An invalid file: URL is an error, as fileURLToPath throws and startup fails.
func resolveCLIResourceFlags(flags CLIFlags, launchCWD string) (CLIFlags, error) {
	resolve := func(paths []string) ([]string, error) {
		out := make([]string, len(paths))
		for i, path := range paths {
			out[i] = path
			if codingagent.IsLocalPath(path) {
				resolved, err := resolvepath.Resolve(path, launchCWD)
				if err != nil {
					return nil, err
				}
				out[i] = resolved
			}
		}
		return out, nil
	}
	var err error
	if flags.Extensions, err = resolve(flags.Extensions); err != nil {
		return flags, err
	}
	if flags.Skills, err = resolve(flags.Skills); err != nil {
		return flags, err
	}
	if flags.PromptTemplates, err = resolve(flags.PromptTemplates); err != nil {
		return flags, err
	}
	if flags.Themes, err = resolve(flags.Themes); err != nil {
		return flags, err
	}
	return flags, nil
}

func resolveSessionDir(flagValue string, sm *codingagent.SettingsManager) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	envName := codingagent.ENV_SESSION_DIR
	if codingagent.UsePiDirs() {
		envName = "PI_CODING_AGENT_SESSION_DIR"
	}
	if envValue := os.Getenv(envName); envValue != "" {
		return codingagent.ExpandTildePath(envValue), nil
	}
	if sm != nil {
		return sm.GetSessionDir()
	}
	return "", nil
}

// configureHTTPDispatcherFromSettings applies the resolved provider request
// settings to the ai package: the effective per-request timeout onto the
// net/http idle dispatcher, and maxRetries/maxRetryDelayMs onto the provider
// retry transport.
func configureHTTPDispatcherFromSettings(sm *codingagent.SettingsManager) error {
	if sm == nil {
		return ai.ConfigureHTTPDispatcher(ai.DefaultHTTPIdleTimeoutMs)
	}
	if err := ai.ApplyHTTPProxySettings(sm.GetGlobalSettings().HTTPProxy); err != nil {
		return err
	}
	// retry.provider.timeoutMs overrides httpIdleTimeoutMs when set
	// (upstream sdk.ts:311).
	timeoutMs, err := sm.GetProviderRequestTimeoutMs()
	if err != nil {
		return err
	}
	if err := ai.ConfigureHTTPDispatcher(timeoutMs); err != nil {
		return err
	}
	// retry.provider.maxRetries / maxRetryDelayMs drive the provider retry
	// transport (upstream sdk.ts passes both into retryProviderRequest).
	pr := sm.GetProviderRetrySettings()
	return ai.ConfigureProviderRetry(pr.MaxRetries, pr.MaxRetryDelayMs)
}

func newSessionManagerWithDir(cwd, sessionDir string) *codingagent.SessionManager {
	if sessionDir != "" {
		return codingagent.NewSessionManagerWithDir(cwd, sessionDir)
	}
	return codingagent.NewSessionManager(cwd)
}

// isValidSessionID validates session ID format.
// Mirrors upstream assertValidSessionId (session-manager.ts).
var validSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`)

func isValidSessionID(id string) bool {
	return validSessionIDPattern.MatchString(id)
}

// exportOfflineMode mirrors upstream main.ts, which normalizes --offline or a
// truthy PI_OFFLINE to PI_OFFLINE=1 and PI_SKIP_VERSION_CHECK=1 in the process
// environment. Pi extensions read PI_OFFLINE (pi-auto-update skips its
// `pi update` run) and inherit this environment, so PiG's own PIG_OFFLINE
// alias sets it too.
func exportOfflineMode(flagOffline bool) {
	if flagOffline || truthyEnvFlag(os.Getenv("PI_OFFLINE")) || truthyEnvFlag(strings.TrimSpace(os.Getenv("PIG_OFFLINE"))) {
		_ = os.Setenv("PI_OFFLINE", "1")
		_ = os.Setenv("PI_SKIP_VERSION_CHECK", "1")
	}
}

// truthyEnvFlag mirrors upstream main.ts isTruthyEnvFlag: "1", "true" or
// "yes", case-insensitively.
func truthyEnvFlag(value string) bool {
	lower := strings.ToLower(value)
	return value == "1" || lower == "true" || lower == "yes"
}
