// Package aiconfig loads and applies the product-level configuration for
// which LLM and decision providers a product wires up: cloud or BYOK (bring
// your own key, connecting directly to the provider, never via the cloud),
// and which deciders run before/instead of the cloud's decision service.
//
// # Cost control for the decision chain
//
// A decision chain can bill a paid engine on every turn without anyone noticing:
// TypeSafe's 429 cannot be told from an exhausted account, so a "fallback" engine
// whose primary is a spent Jev account sends every call to its paid backup.
// Decision.BackupBudget (compose.NewBudget) caps that backup per window, a spent
// cap ends the chain loudly (Decision.StopOnQuota, default on), and a product MUST
// then check decision.Trace.StoppedBy before escalating to its paid main LLM.
// For anonymous or public traffic run the calibrated engine alone (no paid backup)
// or with a budgeted backup. Rules (Deps.ExtraDecision) always run first, so a stop
// at an engine never skips them.
//
// This package has no cloud endpoint of its own: the product supplies its
// cloud boundary's base URL (Deps.CloudBaseURL, or Config.Cloud.BaseURL to
// override it), and BYOK connects straight to the chosen provider.
package aiconfig

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/anthropic"
	"github.com/strongo/aichat/ai/cloud"
	"github.com/strongo/aichat/ai/decision"
	"github.com/strongo/aichat/ai/decision/compose"
	"github.com/strongo/aichat/ai/openaicompat"
)

// LLM configures which chat provider a product uses.
type LLM struct {
	// Provider is "cloud" (default) or "byok".
	Provider string `yaml:"provider" json:"provider"`
}

// Decision configures which decision providers run.
type Decision struct {
	// Provider is "auto" (default: run configured extras, then cloud if a
	// token source is available -- silently skipping cloud decision when
	// one isn't, since "auto" means best-effort), "cloud" (REQUIRE cloud
	// decision on -- Build errors if no token source is available), or
	// "disabled". Any other value is a Build error.
	Provider string `yaml:"provider" json:"provider"`

	// Engines names the decision engines to run, resolved against Deps.Engines
	// (for example ["jev", "llm-decider"]). Which engines run, and how they are
	// combined, is configuration, so the engine behind a decision can be swapped
	// without touching code. Empty: no configured engines (only the product's
	// ExtraDecision providers and the cloud decider). Engines run after the
	// product's own rules and before the cloud decider.
	Engines []string `yaml:"engines" json:"engines"`
	// Strategy combines the engines: "single" (exactly one engine; the default
	// for one), "fallback" (exactly two; the backup runs only if the primary
	// fails; the default for two), "hedged" (exactly two; the backup starts after
	// HedgeAfter or as soon as the primary fails, and the first valid answer
	// wins) or "race" (two or more; the first valid answer wins). More than two
	// engines need "race".
	Strategy string `yaml:"strategy" json:"strategy"`
	// HedgeAfter is the latency budget of the "hedged" strategy as a Go duration
	// such as "600ms" (default compose.DefaultHedgeAfter). Set it above the
	// primary's p99: a primary slower than the budget is paid for twice on every
	// slow call.
	HedgeAfter string `yaml:"hedgeAfter" json:"hedgeAfter"`
	// Breaker wraps every engine in a circuit breaker so an engine that is down
	// is not called on every request. Default true.
	Breaker *bool `yaml:"breaker" json:"breaker"`
	// BreakerSlowThreshold, when above 0, makes that many consecutive calls in
	// which a "hedged" primary was cancelled by its backup open the primary's
	// breaker (compose.WithBreakerSlowThreshold). Default 0: slowness never opens
	// a breaker, only failures (including the primary's own timeout) do. Set it
	// for an engine that can hang without ever failing.
	BreakerSlowThreshold int `yaml:"breakerSlowThreshold" json:"breakerSlowThreshold"`
	// FallbackOn lists extra conditions that start the backup beyond failure:
	// "abstain", "uncertain" and "quota" (the primary refused because the caller's
	// allowance is exhausted; the backup is usually a paid engine, so this is a
	// spending decision). Empty by default: an abstention or an uncertain answer
	// is an answer, not a failure, and an exhausted allowance is surfaced to the
	// caller. A misconfigured endpoint never starts a backup.
	FallbackOn []string `yaml:"fallbackOn" json:"fallbackOn"`
	// Policy names the selection policy applied to calibrated scored answers:
	// "narrowing" or "durable" (see decision.NarrowingPolicy, DurablePolicy).
	// Empty: no policy; the chain keeps its MinConfidence behaviour. The named
	// values are PROVISIONAL defaults from a single sample: re-measure them for
	// your corpus and the exact model id, and set the result in PolicyValues.
	Policy string `yaml:"policy" json:"policy"`
	// PolicyValues overrides individual numbers of the named policy (custom
	// values; any left unset keep the named policy's). The result is validated
	// (decision.SelectionPolicy.Validate): an invalid combination is a Build
	// error. It needs Policy to name the base.
	PolicyValues *PolicyValues `yaml:"policyValues" json:"policyValues"`
	// BackupBudget caps how many calls the BACKUP engine (the second engine of
	// strategy "fallback" or "hedged") may take per window; see BackupBudget.
	// It closes a hole that no engine's own errors can: when the primary fails in a
	// way that looks transient (a TypeSafe 429 is always treated as a rate limit, so
	// an exhausted account looks like one), a fallback hands EVERY call to the
	// backup, and a paid backup (an LLM decider) has no other bound. With a budget,
	// the call after the cap fails with decision.ErrBudget and the chain stops
	// (StopOnQuota), loudly. For anonymous or public traffic run the calibrated
	// engine alone with no paid backup, or with a budgeted one. Unset: no cap.
	BackupBudget *BackupBudget `yaml:"backupBudget" json:"backupBudget"`
	// StopOnQuota (default true) makes the decision chain (Providers.Chain) stop,
	// instead of calling the next provider, when a provider reports an exhausted
	// allowance (decision.ErrQuota) or a spent budget (decision.ErrBudget):
	// exhaustion must be loud and must not silently bill a paid provider behind it,
	// which matters for any anonymous or metered demo. Set false to opt out, naming
	// the decision. A stopped chain returns ok=false with Trace.StoppedBy and
	// Trace.Err: the product MUST check StoppedBy before escalating to its paid main
	// LLM. Combining it explicitly with fallbackOn "quota" (which fails over inside
	// one engine instead) is a Build error: pick one. A stop skips every later
	// provider, so rules (Deps.ExtraDecision) run first.
	StopOnQuota *bool `yaml:"stopOnQuota" json:"stopOnQuota"`
	// StopOnMisconfigured (default true) is the same for a misconfigured engine
	// (decision.ErrMisconfigured: an unknown product, a base URL that does not speak
	// the protocol): a person has to fix it, so it is never absorbed by the next
	// provider.
	StopOnMisconfigured *bool `yaml:"stopOnMisconfigured" json:"stopOnMisconfigured"`
}

// BackupBudget configures compose.NewBudget around the backup engine.
type BackupBudget struct {
	// MaxCalls is the most calls the backup may take per Per window; above 0.
	MaxCalls int `yaml:"maxCalls" json:"maxCalls"`
	// Per is the window as a Go duration such as "1h". Empty or "0": the cap never
	// resets for the life of the process.
	Per string `yaml:"per" json:"per"`
}

// PolicyValues are optional numeric overrides of a named selection policy; see
// decision.SelectionPolicy for what each one means.
type PolicyValues struct {
	MinConfidence        *float64 `yaml:"minConfidence" json:"minConfidence"`
	MinGap               *float64 `yaml:"minGap" json:"minGap"`
	MinProbability       *float64 `yaml:"minProbability" json:"minProbability"`
	StrongProbability    *float64 `yaml:"strongProbability" json:"strongProbability"`
	PotentialProbability *float64 `yaml:"potentialProbability" json:"potentialProbability"`
	MaxPicks             *int     `yaml:"maxPicks" json:"maxPicks"`
	// AcceptUncalibratedAt is the explicit opt-in to acting on an uncalibrated
	// decision (decision.SelectionPolicy.AcceptUncalibratedAt): 0 never, as the
	// durable policy defaults to; the narrowing policy defaults to 0.70.
	AcceptUncalibratedAt *float64 `yaml:"acceptUncalibratedAt" json:"acceptUncalibratedAt"`
	// AcceptUncalibratedSideEffects is the separate opt-in to accepting an
	// uncalibrated side-effectful interaction (confirmation, rejection,
	// correction, cancellation, undo; decision.SelectionPolicy.AcceptUncalibratedSideEffects).
	// False by default for both named policies.
	AcceptUncalibratedSideEffects *bool `yaml:"acceptUncalibratedSideEffects" json:"acceptUncalibratedSideEffects"`
}

// BYOK configures a direct, product-owned connection to an LLM provider.
// BYOK never goes through the cloud boundary.
type BYOK struct {
	// Protocol is "openai-compatible" (default) or "anthropic".
	Protocol string `yaml:"protocol" json:"protocol"`
	// Endpoint is the provider's API base URL. Empty uses a sensible
	// default for the chosen Protocol (https://api.openai.com/v1/ for
	// openai-compatible, https://api.anthropic.com -- bare host, no /v1 --
	// for anthropic; ai/anthropic appends its own fixed "/v1/messages", so
	// baking "/v1" into this default too would double it) -- set it to
	// point BYOK at a self-hosted or third-party
	// OpenAI-compatible/Anthropic-compatible endpoint instead.
	Endpoint string `yaml:"endpoint" json:"endpoint"`
	Model    string `yaml:"model" json:"model"`
	// APIKeyEnv names the environment variable holding the API key. The key
	// value itself is never stored in Config. Leaving it empty is valid
	// (some endpoints need no key); naming a variable that turns out to be
	// unset or empty at Build time is a Build error, since that is almost
	// always a misconfiguration rather than an intentional no-auth setup.
	APIKeyEnv string `yaml:"apiKeyEnv" json:"apiKeyEnv"`
}

// Cloud configures the product's AI cloud boundary. BaseURL overrides
// Deps.CloudBaseURL when set; leave it empty to use the product's default.
type Cloud struct {
	BaseURL string `yaml:"baseURL" json:"baseURL"`
}

// Config is the product-level AI configuration, loaded from YAML/JSON and
// adjustable from environment variables.
type Config struct {
	LLM      LLM      `yaml:"llm" json:"llm"`
	Decision Decision `yaml:"decision" json:"decision"`
	BYOK     BYOK     `yaml:"byok" json:"byok"`
	Cloud    Cloud    `yaml:"cloud" json:"cloud"`

	// envErrs are environment values ApplyEnv could not read (a malformed boolean
	// or number); Build reports them, so a typo in a stop switch is loud.
	envErrs []error
}

// Default BYOK endpoints, used when BYOK.Endpoint is empty. Not cloud
// boundary defaults -- see Cloud's doc and Deps.CloudBaseURL for that.
//
// The two adapters compose their request path differently, so their
// defaults are NOT parallel strings: ai/openaicompat's path is just
// "/chat/completions" appended to BaseURL, so BaseURL itself must already
// include "/v1/" (OpenAI's actual API root). ai/anthropic's path is the
// fixed "/v1/messages" appended to BaseURL, so BaseURL here must be the
// BARE host with no "/v1" of its own -- appending it here too would
// double it into ".../v1/v1/messages".
const (
	defaultOpenAIEndpoint    = "https://api.openai.com/v1/"
	defaultAnthropicEndpoint = "https://api.anthropic.com"
)

// Environment variable SUFFIXES ApplyEnv reads, each joined to the prefix
// passed to ApplyEnv (e.g. prefix "SNEAT_" + EnvLLMProvider ->
// "SNEAT_AI_LLM_PROVIDER"). Documented here since they are the
// product-facing configuration surface.
const (
	EnvLLMProvider   = "AI_LLM_PROVIDER"
	EnvDecision      = "AI_DECISION_PROVIDER"
	EnvBYOKProtocol  = "AI_BYOK_PROTOCOL"
	EnvBYOKEndpoint  = "AI_BYOK_ENDPOINT"
	EnvBYOKModel     = "AI_BYOK_MODEL"
	EnvBYOKAPIKeyEnv = "AI_BYOK_API_KEY_ENV"
	EnvCloudBaseURL  = "AI_CLOUD_BASE_URL"

	// EnvDecisionEngines is a comma-separated list of engine names.
	EnvDecisionEngines    = "AI_DECISION_ENGINES"
	EnvDecisionStrategy   = "AI_DECISION_STRATEGY"
	EnvDecisionHedgeAfter = "AI_DECISION_HEDGE_AFTER"
	EnvDecisionPolicy     = "AI_DECISION_POLICY"
	// EnvDecisionStopOnQuota and EnvDecisionStopOnMisconfigured are booleans as
	// strconv.ParseBool reads them (1, t, T, TRUE, true, True, 0, f, F, FALSE, false,
	// False); anything else, a padded value included, is a Build error.
	EnvDecisionStopOnQuota         = "AI_DECISION_STOP_ON_QUOTA"
	EnvDecisionStopOnMisconfigured = "AI_DECISION_STOP_ON_MISCONFIGURED"
	// EnvDecisionBackupBudgetMaxCalls (an integer) and EnvDecisionBackupBudgetPer (a
	// Go duration) set Decision.BackupBudget; either alone creates it.
	EnvDecisionBackupBudgetMaxCalls = "AI_DECISION_BACKUP_BUDGET_MAX_CALLS"
	EnvDecisionBackupBudgetPer      = "AI_DECISION_BACKUP_BUDGET_PER"
)

func defaults() Config {
	return Config{
		LLM:      LLM{Provider: "cloud"},
		Decision: Decision{Provider: "auto"},
	}
}

// Load reads Config from path (YAML). A missing file returns defaults, not
// an error, so a product can ship with no config file at all.
func Load(path string) (Config, error) {
	cfg := defaults()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("aiconfig: read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("aiconfig: parse %s: %w", path, err)
	}
	fillDefaults(&cfg)
	return cfg, nil
}

func fillDefaults(cfg *Config) {
	if cfg.LLM.Provider == "" {
		cfg.LLM.Provider = "cloud"
	}
	if cfg.Decision.Provider == "" {
		cfg.Decision.Provider = "auto"
	}
}

// ApplyEnv overrides Config fields from environment variables named
// prefix+Env* (see the Env* constants), using getenv so callers/tests don't
// need real process env. prefix lets each product namespace its own copy of
// these variables (e.g. "SNEAT_" for SNEAT_AI_LLM_PROVIDER); pass "" for an
// unprefixed/shared deployment.
func (c *Config) ApplyEnv(getenv func(string) string, prefix string) {
	if getenv == nil {
		return
	}
	get := func(suffix string) string { return getenv(prefix + suffix) }
	if v := get(EnvLLMProvider); v != "" {
		c.LLM.Provider = v
	}
	if v := get(EnvDecision); v != "" {
		c.Decision.Provider = v
	}
	if v := get(EnvDecisionEngines); v != "" {
		c.Decision.Engines = splitList(v)
	}
	if v := get(EnvDecisionStrategy); v != "" {
		c.Decision.Strategy = v
	}
	if v := get(EnvDecisionHedgeAfter); v != "" {
		c.Decision.HedgeAfter = v
	}
	if v := get(EnvDecisionPolicy); v != "" {
		c.Decision.Policy = v
	}
	c.applyStopAndBudgetEnv(get)
	if v := get(EnvBYOKProtocol); v != "" {
		c.BYOK.Protocol = v
	}
	if v := get(EnvBYOKEndpoint); v != "" {
		c.BYOK.Endpoint = v
	}
	if v := get(EnvBYOKModel); v != "" {
		c.BYOK.Model = v
	}
	if v := get(EnvBYOKAPIKeyEnv); v != "" {
		c.BYOK.APIKeyEnv = v
	}
	if v := get(EnvCloudBaseURL); v != "" {
		c.Cloud.BaseURL = v
	}
}

// applyStopAndBudgetEnv reads the stop switches and the backup budget. A value it
// cannot parse is remembered for Build to report and leaves the field alone.
func (c *Config) applyStopAndBudgetEnv(get func(string) string) {
	boolEnv := func(name string, dst **bool) {
		v := get(name)
		if v == "" {
			return
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			c.envError(fmt.Errorf("aiconfig: %s=%q is not a boolean", name, v))
			return
		}
		*dst = &b
	}
	boolEnv(EnvDecisionStopOnQuota, &c.Decision.StopOnQuota)
	boolEnv(EnvDecisionStopOnMisconfigured, &c.Decision.StopOnMisconfigured)
	budget := func() *BackupBudget {
		if c.Decision.BackupBudget == nil {
			c.Decision.BackupBudget = &BackupBudget{}
		}
		return c.Decision.BackupBudget
	}
	if v := get(EnvDecisionBackupBudgetMaxCalls); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			c.envError(fmt.Errorf("aiconfig: %s=%q is not an integer", EnvDecisionBackupBudgetMaxCalls, v))
		} else {
			budget().MaxCalls = n
		}
	}
	if v := get(EnvDecisionBackupBudgetPer); v != "" {
		budget().Per = v
	}
}

// envError records an unreadable environment value once (applying the same
// environment twice, by the caller and by Build, must not repeat it).
func (c *Config) envError(err error) {
	for _, e := range c.envErrs {
		if e.Error() == err.Error() {
			return
		}
	}
	c.envErrs = append(c.envErrs, err)
}

// String renders Config for logs/diagnostics. It never prints key values --
// only APIKeyEnv (the variable NAME) is shown, never read.
func (c Config) String() string {
	return fmt.Sprintf(
		"Config{LLM.Provider:%s Decision.Provider:%s Decision.Engines:%v Decision.Strategy:%s BYOK.Protocol:%s BYOK.Endpoint:%s BYOK.Model:%s BYOK.APIKeyEnv:%s Cloud.BaseURL:%s}",
		c.LLM.Provider, c.Decision.Provider, c.Decision.Engines, c.Decision.Strategy, c.BYOK.Protocol, c.BYOK.Endpoint, c.BYOK.Model, c.BYOK.APIKeyEnv, c.Cloud.BaseURL,
	)
}

// Deps are the runtime dependencies Build needs beyond Config: things that
// can't come from a config file (secrets, endpoints, the product's own
// deterministic deciders).
type Deps struct {
	// Product identifies the consuming product for cloud metering/routing.
	Product       string
	ClientContext *ai.ClientContext
	// CloudToken supplies the bearer token for cloud requests. Required when
	// LLM.Provider=="cloud" or a cloud decision provider is wired up.
	CloudToken func(context.Context) (string, error)
	// CloudBaseURL is the product's AI cloud boundary base URL, used
	// whenever Config.Cloud.BaseURL is empty. This package has no default
	// of its own -- see the package doc.
	CloudBaseURL string
	// EnvPrefix namespaces the AI_* environment variables Config.ApplyEnv
	// reads when Build applies them (e.g. "SNEAT_"). Build always applies
	// env overrides on top of Config using Getenv+EnvPrefix, so a product
	// does not need to call ApplyEnv itself.
	EnvPrefix string
	// Getenv reads environment variables (defaults to os.Getenv).
	Getenv func(string) string
	// HTTPClient is shared by every adapter Build constructs (defaults to
	// http.DefaultClient).
	HTTPClient *http.Client
	// ExtraDecision are the product's own deterministic decision.Provider
	// values (e.g. ai/decision/rules instances). They run BEFORE the cloud
	// decision provider in the chain.
	ExtraDecision []decision.Provider
	// DisableCloudDecision forces the cloud decision provider off (e.g. a
	// product's --no-jev flag) regardless of Config.Decision.Provider.
	DisableCloudDecision bool
	// Engines is the registry Config.Decision.Engines resolves names against:
	// each value is a decision.Provider (and, for scored questions, a
	// decision.ScoredProvider), for example a real decision model and an LLM
	// decider.
	Engines map[string]decision.Provider
	// Clock drives the engines' timeouts, hedge delay and breakers (tests);
	// nil means the real clock.
	Clock compose.Clock
	// OnBreakerChange is called when an engine's circuit breaker changes
	// state, so a host can log and count it.
	OnBreakerChange func(engine string, from, to compose.BreakerState)
}

// Providers is what Build assembles: the single LLM provider to answer chat,
// and the ordered decision-provider chain (feed straight into
// decision.Chain{Providers: providers.Decision}).
type Providers struct {
	LLM      ai.LLMProvider
	Decision []decision.Provider
	// Policy is the selection policy named by Config.Decision.Policy, nil when
	// none: feed it to decision.Chain{Policy: providers.Policy}, or use Chain.
	Policy *decision.SelectionPolicy
	// StopOnQuota and StopOnMisconfigured are Config.Decision.StopOnQuota and
	// StopOnMisconfigured as the decision.Chain settings (zero: stop).
	StopOnQuota, StopOnMisconfigured decision.StopPolicy
}

// Chain is the decision chain Build configured: Decision in order, the Policy,
// and the stop settings. The product's own rules (Deps.ExtraDecision) come first
// and are deterministic: under any Policy, including "durable", a rule match is
// accepted (decision.OutcomeDeterministic) and the engines behind it are not
// called. They must stay first: a chain that stops at an exhausted allowance or
// budget never reaches a provider placed after the one that stopped it. The
// result of Decide with ok=false is NOT always "nobody decided": check
// Trace.StoppedBy before escalating to a paid main LLM. MinConfidence, Timeout and
// KeepNonSelected keep their defaults; set them on the returned value.
func (p Providers) Chain() decision.Chain {
	return decision.Chain{Providers: p.Decision, Policy: p.Policy, StopOnQuota: p.StopOnQuota, StopOnMisconfigured: p.StopOnMisconfigured}
}

// Build wires up Providers from cfg and deps. LLM and Decision are
// independent: a cloud Decision chain works with a BYOK LLM, and vice versa.
//
// Build first applies deps.Getenv/deps.EnvPrefix env overrides on top of cfg
// (see Config.ApplyEnv), so a product only needs to Load once and pass Deps.
func Build(cfg Config, deps Deps) (Providers, error) {
	if deps.Getenv == nil {
		deps.Getenv = os.Getenv
	}
	if deps.HTTPClient == nil {
		deps.HTTPClient = http.DefaultClient
	}
	// Errors an earlier ApplyEnv on this Config recorded are kept: Build reports
	// them too, so a caller that applied the environment itself cannot lose one.
	cfg.ApplyEnv(deps.Getenv, deps.EnvPrefix)
	fillDefaults(&cfg)
	if err := errors.Join(cfg.envErrs...); err != nil {
		return Providers{}, err
	}

	switch cfg.Decision.Provider {
	case "", "auto", "cloud", "disabled":
	default:
		return Providers{}, fmt.Errorf("aiconfig: unknown decision.provider %q", cfg.Decision.Provider)
	}

	var (
		out Providers
		err error
	)
	var cloudClient *cloud.Client
	haveCloudToken := deps.CloudToken != nil
	cloudBaseURL := cfg.Cloud.BaseURL
	if cloudBaseURL == "" {
		cloudBaseURL = deps.CloudBaseURL
	}
	// buildCloudClient constructs (and caches) the cloud.Client
	// unconditionally: it never fails, because its ONLY caller
	// (newCloudClient, below) invokes it after already having validated
	// haveCloudToken/cloudBaseURL itself. Keeping construction error-free
	// lets a caller that has independently proven those preconditions true
	// (the "auto" case below) call it directly instead of having to check
	// (and discard) an error that can't occur on that path.
	buildCloudClient := func() *cloud.Client {
		if cloudClient == nil {
			cloudClient = cloud.New(cloud.Config{
				BaseURL:       cloudBaseURL,
				Product:       deps.Product,
				ClientContext: deps.ClientContext,
				Token:         deps.CloudToken,
				HTTPClient:    deps.HTTPClient,
			})
		}
		return cloudClient
	}
	newCloudClient := func() (*cloud.Client, error) {
		if cloudClient != nil {
			return cloudClient, nil
		}
		if !haveCloudToken {
			return nil, fmt.Errorf("aiconfig: cloud provider requested but Deps.CloudToken is nil")
		}
		if cloudBaseURL == "" {
			return nil, fmt.Errorf("aiconfig: cloud provider requested but no base URL (set Config.Cloud.BaseURL or Deps.CloudBaseURL)")
		}
		return buildCloudClient(), nil
	}

	switch cfg.LLM.Provider {
	case "", "cloud":
		c, err := newCloudClient()
		if err != nil {
			return Providers{}, err
		}
		out.LLM = c
	case "byok":
		llm, err := buildBYOK(cfg.BYOK, deps)
		if err != nil {
			return Providers{}, err
		}
		out.LLM = llm
	default:
		return Providers{}, fmt.Errorf("aiconfig: unknown llm.provider %q", cfg.LLM.Provider)
	}

	out.Decision = append(out.Decision, deps.ExtraDecision...)

	if out.Policy, err = policyFromConfig(cfg.Decision); err != nil {
		return Providers{}, err
	}
	if err := checkStopConfig(cfg.Decision); err != nil {
		return Providers{}, err
	}
	out.StopOnQuota, out.StopOnMisconfigured = stopFromConfig(cfg.Decision.StopOnQuota), stopFromConfig(cfg.Decision.StopOnMisconfigured)
	if cfg.Decision.Provider != "disabled" {
		engine, err := buildEngine(cfg.Decision, deps, out.Policy)
		if err != nil {
			return Providers{}, err
		}
		if engine != nil {
			out.Decision = append(out.Decision, engine)
		}
	}

	switch {
	case deps.DisableCloudDecision || cfg.Decision.Provider == "disabled":
		// cloud decision off; nothing to do.
	case cfg.Decision.Provider == "cloud":
		// Explicitly requested: no token source is a hard error, unlike
		// "auto" which skips silently.
		c, err := newCloudClient()
		if err != nil {
			return Providers{}, fmt.Errorf("aiconfig: decision.provider=cloud: %w", err)
		}
		out.Decision = append(out.Decision, c.Decider())
	case haveCloudToken && cloudBaseURL != "":
		// "auto" (or "", which fillDefaults already turned into "auto"):
		// best-effort -- wire cloud decision in only when we plainly can.
		// Both of newCloudClient's failure preconditions are already false
		// here, so call the never-fails constructor directly instead of
		// checking (and discarding) an error that can't occur on this path.
		out.Decision = append(out.Decision, buildCloudClient().Decider())
	}

	return out, nil
}

func buildBYOK(cfg BYOK, deps Deps) (ai.LLMProvider, error) {
	protocol := strings.ToLower(cfg.Protocol)
	if protocol == "" {
		protocol = "openai-compatible"
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		switch protocol {
		case "openai-compatible":
			endpoint = defaultOpenAIEndpoint
		case "anthropic":
			endpoint = defaultAnthropicEndpoint
		}
	}
	if endpoint == "" {
		return nil, fmt.Errorf("aiconfig: byok.endpoint is required when llm.provider=byok and byok.protocol %q has no default", cfg.Protocol)
	}
	apiKey, err := resolveAPIKey(cfg.APIKeyEnv, deps.Getenv)
	if err != nil {
		return nil, err
	}
	switch protocol {
	case "openai-compatible":
		// ai/openaicompat has no built-in default model (unlike
		// ai/anthropic, which falls back to a Haiku default): an empty
		// Model here would only fail later, at request time, against the
		// real API. Catching it now is a Build-time error instead.
		if cfg.Model == "" {
			return nil, fmt.Errorf("aiconfig: byok.model is required when byok.protocol is openai-compatible (no built-in default)")
		}
		return openaicompat.New(openaicompat.Config{
			BaseURL:    endpoint,
			APIKey:     apiKey,
			Model:      cfg.Model,
			HTTPClient: deps.HTTPClient,
		}), nil
	case "anthropic":
		return anthropic.New(anthropic.Config{
			BaseURL:    endpoint,
			APIKey:     apiKey,
			Model:      cfg.Model,
			HTTPClient: deps.HTTPClient,
		}), nil
	default:
		return nil, fmt.Errorf("aiconfig: unknown byok.protocol %q", cfg.Protocol)
	}
}

// resolveAPIKey reads the API key named by apiKeyEnv. Naming no variable at
// all is valid (some endpoints need no key); naming one that resolves to ""
// is a Build error, since that is almost always a forgotten export rather
// than an intentional no-auth setup.
func resolveAPIKey(apiKeyEnv string, getenv func(string) string) (string, error) {
	if apiKeyEnv == "" {
		return "", nil
	}
	v := getenv(apiKeyEnv)
	if v == "" {
		return "", fmt.Errorf("aiconfig: byok.apiKeyEnv names %q but it is unset or empty", apiKeyEnv)
	}
	return v, nil
}
