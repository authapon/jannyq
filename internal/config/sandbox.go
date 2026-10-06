package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

// SandboxConfig holds the settings of the `jannyq sandbox` command executor.
// Its variables are prefixed JANNYQ_SANDBOX_ (flag --max-timeout is
// JANNYQ_SANDBOX_MAX_TIMEOUT); the token is shared with the bot's own
// JANNYQ_SANDBOX_TOKEN.
type SandboxConfig struct {
	Listen         string
	Token          string
	WorkDir        string
	UIDBase        int
	UIDCount       int
	AllowRoot      bool
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
	MaxOutputKB    int
	MaxFileMB      int
	CPUSeconds     int
	OpenFiles      int
	MaxProcs       int
	QuotaMB        int
	MaxConcurrent  int
	IdleTTL        time.Duration
	Network        string // "on" or "off": what the surrounding container provides
	Shell          string
	LogLevel       string
	LogJSON        bool
}

// LoadSandbox parses the arguments of `jannyq sandbox` (without the
// subcommand) and the JANNYQ_SANDBOX_* environment.
func LoadSandbox(args []string, env func(string) string, stderr io.Writer) (*SandboxConfig, error) {
	c := &SandboxConfig{}
	fs := flag.NewFlagSet("jannyq sandbox", flag.ContinueOnError)
	fs.SetOutput(stderr)
	l := &loader{fs: fs, env: env, prefix: "JANNYQ_SANDBOX_"}

	l.str(&c.Listen, "listen", ":9090", "address of the executor's HTTP API")
	l.secret(&c.Token, "token", "shared secret the bot must present (at least 16 characters)")
	l.str(&c.WorkDir, "workdir", "/work", "directory holding the per-chat workspaces")
	l.integer(&c.UIDBase, "uid-base", 20000, "first user id handed out to workspaces (needs root)")
	l.integer(&c.UIDCount, "uid-count", 40000, "number of user ids available to workspaces; 0 disables per-workspace users")
	l.boolean(&c.AllowRoot, "allow-root", false, "run commands as the executor's own user even if that is root (DANGEROUS)")
	l.duration(&c.DefaultTimeout, "default-timeout", 30*time.Second, "command timeout when the request does not give one")
	l.duration(&c.MaxTimeout, "max-timeout", 120*time.Second, "longest command timeout a request may ask for")
	l.integer(&c.MaxOutputKB, "max-output-kb", 64, "output returned per command, in KB (head and tail are kept)")
	l.integer(&c.MaxFileMB, "max-file-mb", 100, "largest single file a command may write, in MB")
	l.integer(&c.CPUSeconds, "cpu-seconds", 0, "CPU time limit per command in seconds; 0 = max-timeout")
	l.integer(&c.OpenFiles, "open-files", 256, "open file limit per command")
	l.integer(&c.MaxProcs, "max-procs", 128, "processes (threads) one workspace's user may have at once, against fork bombs")
	l.integer(&c.QuotaMB, "quota-mb", 256, "size limit per workspace in MB")
	l.integer(&c.MaxConcurrent, "max-concurrent", 4, "commands running at once")
	l.duration(&c.IdleTTL, "idle-ttl", 7*24*time.Hour, "delete workspaces unused for this long")
	l.str(&c.Network, "network", "off", "tell the model whether the sandbox has internet access: on or off (set by the deployment, not enforced here)")
	l.str(&c.Shell, "shell", "/bin/sh", "shell used to run commands")
	l.str(&c.LogLevel, "log-level", "info", "log level: debug, info, warn, error")
	l.boolean(&c.LogJSON, "log-json", false, "log in JSON format")

	fs.Usage = usageFunc(fs, stderr, "jannyq sandbox [flags]")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if len(l.errs) > 0 {
		return nil, errors.Join(l.errs...)
	}
	return c, c.validate()
}

func (c *SandboxConfig) validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if len(c.Token) < 16 {
		bad("--token (JANNYQ_SANDBOX_TOKEN) must be a secret of at least 16 characters, e.g. from `openssl rand -hex 32`")
	}
	if c.WorkDir == "" {
		bad("--workdir must not be empty")
	}
	if c.UIDCount < 0 || c.UIDBase < 1000 && c.UIDCount > 0 {
		bad("--uid-base must be at least 1000 and --uid-count must not be negative")
	}
	if c.UIDBase+c.UIDCount > 1<<31-1 {
		bad("--uid-base + --uid-count is too large")
	}
	if c.DefaultTimeout <= 0 || c.MaxTimeout <= 0 || c.DefaultTimeout > c.MaxTimeout {
		bad("--default-timeout and --max-timeout must be positive, with default <= max")
	}
	for name, v := range map[string]int{
		"max-output-kb": c.MaxOutputKB, "max-file-mb": c.MaxFileMB, "open-files": c.OpenFiles,
		"quota-mb": c.QuotaMB, "max-concurrent": c.MaxConcurrent, "max-procs": c.MaxProcs,
	} {
		if v < 1 {
			bad("--%s must be at least 1", name)
		}
	}
	if c.CPUSeconds < 0 {
		bad("--cpu-seconds must not be negative")
	}
	if c.Network != "on" && c.Network != "off" {
		bad("--network must be on or off")
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		bad("--log-level must be debug, info, warn or error")
	}
	return errors.Join(errs...)
}
