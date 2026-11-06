// Command kaab is the Kaabist command-line interface, built on
// the Go SDK (package kaab).
//
// Exit codes: 0 ok, 1 error, 2 job failed (or canceled), 3 job parked, waiting for an input or
// for attention, with a hint on what to run next.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/kaabist/kaab-go/kaab"
	"github.com/spf13/cobra"
)

// version is overridden at build time (-ldflags "-X main.version=...").
var version = "dev"

const (
	exitOK     = 0
	exitError  = 1
	exitFailed = 2
	exitParked = 3
)

// exitError carries a process exit code through cobra.
type exitCodeError struct {
	code int
	err  error
}

func (e *exitCodeError) Error() string {
	if e.err == nil {
		return ""
	}
	return e.err.Error()
}

// env is everything the CLI touches outside itself, so tests can replace it.
type env struct {
	stdout  io.Writer
	stderr  io.Writer
	stdin   io.Reader
	getenv  func(string) string
	secrets secretStore
	open    func(url string) error
	// httpOptions are added to every SDK client (tests point it at a fake).
	clientOptions []kaab.Option
}

// globals are the persistent flags.
type globals struct {
	profile string
	test    bool
	json    bool
	baseURL string
}

type app struct {
	env *env
	g   globals
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

func (a *app) store() (*store, error) {
	dir, err := configDir(a.env.getenv)
	if err != nil {
		return nil, err
	}
	return &store{dir: dir, secrets: a.env.secrets}, nil
}

// profileName is --profile, KAAB_PROFILE, the config's current profile, or "default".
func (a *app) profileName(cfg *Config) string {
	switch {
	case a.g.profile != "":
		return a.g.profile
	case a.env.getenv("KAAB_PROFILE") != "":
		return a.env.getenv("KAAB_PROFILE")
	case cfg.Current != "":
		return cfg.Current
	}
	return defaultProfile
}

// client is the SDK client for the selected profile and mode; KAAB_API_KEY and KAAB_BASE_URL
// win over the profile.
func (a *app) client() (*kaab.Client, *Profile, error) {
	s, err := a.store()
	if err != nil {
		return nil, nil, err
	}
	cfg, err := s.load()
	if err != nil {
		return nil, nil, err
	}
	name := a.profileName(cfg)
	profile := cfg.Profiles[name]
	if profile == nil {
		profile = &Profile{}
	}
	key := a.env.getenv("KAAB_API_KEY")
	if key == "" {
		mode := "live"
		if a.g.test || (!hasMode(profile, "live") && hasMode(profile, "test")) {
			mode = "test"
		}
		key, err = s.getKey(name, mode)
		if err != nil {
			return nil, nil, err
		}
	}
	return kaab.New(key, a.clientOptions(profile)...), profile, nil
}

// clientOptions are the SDK options for profile: --base-url, then KAAB_BASE_URL, then the
// profile's base URL.
func (a *app) clientOptions(profile *Profile) []kaab.Option {
	base := a.g.baseURL
	if base == "" {
		base = a.env.getenv("KAAB_BASE_URL")
	}
	if base == "" && profile != nil {
		base = profile.BaseURL
	}
	opts := append([]kaab.Option{}, a.env.clientOptions...)
	if base != "" {
		opts = append(opts, kaab.WithBaseURL(base))
	}
	return opts
}

func (a *app) panelURL(profile *Profile) string {
	if url := a.env.getenv("KAAB_PANEL_URL"); url != "" {
		return strings.TrimRight(url, "/")
	}
	if profile != nil && profile.PanelURL != "" {
		return strings.TrimRight(profile.PanelURL, "/")
	}
	return defaultPanelURL
}

func newRoot(e *env) *cobra.Command {
	a := &app{env: e}
	root := &cobra.Command{
		Use:           "kaab",
		Short:         "The Kaabist command-line interface",
		Long:          "kaab runs and inspects Kaabist jobs, programs, sessions, proposals, schedules and webhooks.\n\nAuthenticate once with `kaab auth login`; KAAB_API_KEY and KAAB_BASE_URL override the stored profile.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(e.stdout)
	root.SetErr(e.stderr)
	root.SetIn(e.stdin)
	root.SetVersionTemplate("kaab {{.Version}}\n")
	flags := root.PersistentFlags()
	flags.StringVar(&a.g.profile, "profile", "", "profile to use (KAAB_PROFILE; the current profile by default)")
	flags.BoolVar(&a.g.test, "test", false, "use the profile's test-mode key (sk_test_)")
	flags.BoolVar(&a.g.json, "json", false, "print the API's JSON instead of a table")
	flags.StringVar(&a.g.baseURL, "base-url", "", "API base URL (KAAB_BASE_URL; https://api.kaab.ist/v1 by default)")

	root.AddCommand(
		a.authCommand(),
		a.jobsCommand(),
		a.programsCommand(),
		a.sessionsCommand(),
		a.proposalsCommand(),
		a.schedulesCommand(),
		a.webhooksCommand(),
		a.creditsCommand(),
		a.usageCommand(),
		a.openCommand(),
		a.docsCommand(),
		&cobra.Command{
			Use:   "version",
			Short: "Print the CLI's version",
			Args:  cobra.NoArgs,
			Run: func(cmd *cobra.Command, _ []string) {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "kaab %s (sdk %s)\n", version, kaab.Version)
			},
		},
	)
	root.CompletionOptions.HiddenDefaultCmd = false
	return root
}

// run executes the CLI with args and answers the process exit code.
func run(ctx context.Context, args []string, e *env) int {
	root := newRoot(e)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return exitOK
	}
	var coded *exitCodeError
	if errors.As(err, &coded) {
		if coded.err != nil {
			_, _ = fmt.Fprintln(e.stderr, "error:", coded.err)
		}
		return coded.code
	}
	_, _ = fmt.Fprintln(e.stderr, "error:", err)
	return exitError
}

func main() {
	e := &env{
		stdout:  os.Stdout,
		stderr:  os.Stderr,
		stdin:   os.Stdin,
		getenv:  os.Getenv,
		secrets: osKeyring{},
		open:    openBrowser,
	}
	// Ctrl-C and SIGTERM cancel what is in flight, so a download cleans up its temporary file.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], e)
	stop()
	os.Exit(code)
}
