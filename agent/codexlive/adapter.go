package codexlive

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/valbaudo/awf/agent"
	"github.com/valbaudo/awf/agent/codex"
	"github.com/valbaudo/awf/agent/live"
	"github.com/valbaudo/awf/clock"
	"github.com/valbaudo/awf/container"
	"github.com/valbaudo/awf/ir"
	"github.com/valbaudo/awf/pricing"
)

// renamedKeys — with-keys that used to exist under a different name. F12:
// `effort` is now the canonical name (matching anthropic/claude-code and
// openai/codex); `reasoning_effort` no longer exists. Checked BEFORE the
// generic unknown-key loop so the author gets a specific rename pointer.
// KeyUnknown: true so the run-start with:-config guard (U1) surfaces this
// pre-spend, same as any other unknown key.
var renamedKeys = map[string]string{"reasoning_effort": "effort"}

const AdapterRef = "openai/codex-live"

var defaultBackoff = deterministicBackoff(3, 100*time.Millisecond)

type Adapter struct {
	env     agent.SecretEnv
	root    live.Root
	client  Client
	clock   clock.Clock
	backoff []time.Duration
	pricer  pricing.Table // model→rates for the derived USD cost; defaults to pricing.Default()
}

type Option func(*Adapter)

func WithLiveRoot(root live.Root) Option {
	return func(a *Adapter) { a.root = root }
}

func WithEnv(env map[string]string) Option {
	return func(a *Adapter) {
		if len(env) == 0 {
			a.env = agent.SecretEnv{}
			return
		}
		out := make(agent.SecretEnv, len(env))
		for k, v := range env {
			out[k] = v
		}
		a.env = out
	}
}

func WithClient(client Client) Option {
	return func(a *Adapter) { a.client = client }
}

func WithClock(c clock.Clock) Option {
	return func(a *Adapter) {
		if c != nil {
			a.clock = c
		}
	}
}

func WithBackoff(durations []time.Duration) Option {
	return func(a *Adapter) {
		a.backoff = append([]time.Duration(nil), durations...)
	}
}

// WithPricing injects the pricing.Table used to derive a USD cost from token
// usage. Tests pass a self-contained fixture table; production leaves it unset so
// New defaults it to pricing.Default() (embedded rates ⊕ $AWF_PRICING_FILE).
func WithPricing(t pricing.Table) Option {
	return func(a *Adapter) { a.pricer = t }
}

func New(opts ...Option) (*Adapter, error) {
	a := &Adapter{
		env:     agent.SecretEnv{},
		clock:   clock.System{},
		backoff: append([]time.Duration(nil), defaultBackoff...),
	}
	for _, opt := range opts {
		opt(a)
	}
	if a.client == nil {
		a.client = newProcessClient(a.env)
	}
	if a.pricer == nil {
		a.pricer = pricing.Default()
	}
	return a, nil
}

func (*Adapter) Ref() string { return AdapterRef }

func (*Adapter) Capabilities() agent.Caps {
	return agent.Caps{
		NativeSchema:      true,
		Containerless:     true,
		PersistentSession: true,
	}
}

// RequiredEnv implements agent.CredentialNamer. Returns the CREDENTIAL env var
// name codex-live authenticates with. OPENAI_API_KEY is defined in
// DefaultEnvAllowlist; CODEX_HOME is a config directory (not a credential) and
// is intentionally excluded.
func (*Adapter) RequiredEnv() []string {
	return []string{"OPENAI_API_KEY"}
}

func (a *Adapter) Version(ctx context.Context, _ container.Handle) (string, error) {
	if a.client == nil {
		return "", errors.New("agent/codexlive: Version: no app-server client wired")
	}
	info, err := a.client.ProviderInfo(ctx)
	if err != nil {
		return "", err
	}
	return live.FormatVersion(info.Version, AppServerSchemaDigest()), nil
}

func (a *Adapter) ValidateConfig(with ir.RawConfig) error {
	cfg, err := parseConfig(with)
	if err != nil {
		return err
	}
	_, err = parsePermissionPolicy(cfg.permissionRaw)
	return err
}

func (a *Adapter) PreflightResume(ctx context.Context, req agent.LiveResumePreflightRequest) error {
	if err := a.requireRootAndClient(); err != nil {
		return err
	}
	cfg, err := parseConfig(req.With)
	if err != nil {
		return err
	}
	rec, err := live.ReadSessionRecord(a.root, AdapterRef, cfg.session)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if rec.ActiveTurn != nil {
		return agent.ErrLiveReplayRequired
	}
	return ctx.Err()
}

func (a *Adapter) requireRootAndClient() error {
	if a.root.Path == "" {
		return errors.New("agent/codexlive: live root required (use WithLiveRoot)")
	}
	if a.client == nil {
		return errors.New("agent/codexlive: app-server client required (use WithClient)")
	}
	return nil
}

type config struct {
	prompt          string
	cwd             string
	canonicalCWD    string
	session         string
	model           string
	reasoningEffort string
	permissionRaw   any
}

func parseConfig(with ir.RawConfig) (config, error) {
	allowed := map[string]struct{}{
		"prompt": {}, "cwd": {}, "session": {}, "model": {}, "effort": {}, "permission_policy": {},
	}
	for old, newKey := range renamedKeys {
		if _, ok := with[old]; ok {
			return config{}, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: old, Reason: "renamed to " + newKey, KeyUnknown: true}
		}
	}
	for k := range with {
		if _, ok := allowed[k]; !ok {
			return config{}, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: k, Reason: "unknown with-key", KeyUnknown: true}
		}
	}
	prompt, err := requiredString(with, "prompt")
	if err != nil {
		return config{}, err
	}
	cwd, err := requiredString(with, "cwd")
	if err != nil {
		return config{}, err
	}
	if err := validateCWDValue(cwd); err != nil {
		return config{}, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: "cwd", Reason: err.Error()}
	}
	session, err := requiredString(with, "session")
	if err != nil {
		return config{}, err
	}
	if err := live.ValidateSessionKey(session); err != nil {
		return config{}, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: "session", Reason: err.Error()}
	}
	cfg := config{prompt: prompt, cwd: cwd, session: session}
	if v, ok := with["model"]; ok {
		s, ok := v.(string)
		if !ok {
			return config{}, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: "model", Reason: fmt.Sprintf("must be string, got %T", v)}
		}
		cfg.model = s
	}
	if v, ok := with["effort"]; ok {
		s, ok := v.(string)
		if !ok {
			return config{}, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: "effort", Reason: fmt.Sprintf("must be string, got %T", v)}
		}
		// codexlive wraps the same codex CLI, so it shares codex's six-value
		// model_reasoning_effort enum (single-sourced as codex.EffortValues) —
		// unlike the with-key-only checks above, this one was previously
		// missing entirely (F12), letting a bad value reach the app-server and
		// fail mid-run instead of at validate time.
		if !slices.Contains(codex.EffortValues, s) {
			return config{}, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: "effort", Reason: fmt.Sprintf("must be one of %v, got %q", codex.EffortValues, s)}
		}
		cfg.reasoningEffort = s
	}
	cfg.permissionRaw = with["permission_policy"]
	return cfg, nil
}

func withCanonicalCWD(cfg config) (config, error) {
	canon, err := filepath.EvalSymlinks(filepath.Clean(cfg.cwd))
	if err != nil {
		return config{}, &agent.ErrInvalidConfig{Ref: AdapterRef, Key: "cwd", Reason: err.Error()}
	}
	cfg.canonicalCWD = canon
	return cfg, nil
}

func validateCWDValue(cwd string) error {
	if !filepath.IsAbs(cwd) {
		return errors.New("must be absolute")
	}
	if !utf8.ValidString(cwd) || strings.ContainsRune(cwd, '\x00') {
		return errors.New("must be valid UTF-8 without NUL bytes")
	}
	for _, r := range cwd {
		if r < 0x20 || r == 0x7f {
			return errors.New("must not contain control characters")
		}
	}
	if filepath.Clean(cwd) != cwd {
		return errors.New("must be clean")
	}
	return nil
}

func requiredString(with ir.RawConfig, key string) (string, error) {
	v, ok := with[key]
	if !ok {
		return "", &agent.ErrInvalidConfig{Ref: AdapterRef, Key: key, Reason: "required"}
	}
	s, ok := v.(string)
	if !ok {
		return "", &agent.ErrInvalidConfig{Ref: AdapterRef, Key: key, Reason: fmt.Sprintf("must be string, got %T", v)}
	}
	if s == "" {
		return "", &agent.ErrInvalidConfig{Ref: AdapterRef, Key: key, Reason: "must not be empty"}
	}
	return s, nil
}

func cloneRawConfig(in ir.RawConfig) ir.RawConfig {
	out := make(ir.RawConfig, len(in))
	maps.Copy(out, in)
	return out
}
