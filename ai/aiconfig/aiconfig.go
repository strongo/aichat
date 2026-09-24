// Package aiconfig loads and applies the product-level configuration for
// which LLM and decision providers a product wires up: cloud or BYOK (bring
// your own key, connecting directly to the provider, never via the cloud),
// and which deciders run before/instead of the cloud's decision service.
//
// This package has no cloud endpoint of its own: the product supplies its
// cloud boundary's base URL (Deps.CloudBaseURL, or Config.Cloud.BaseURL to
// override it), and BYOK connects straight to the chosen provider.
package aiconfig

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/anthropic"
	"github.com/strongo/aichat/ai/cloud"
	"github.com/strongo/aichat/ai/decision"
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

// String renders Config for logs/diagnostics. It never prints key values --
// only APIKeyEnv (the variable NAME) is shown, never read.
func (c Config) String() string {
	return fmt.Sprintf(
		"Config{LLM.Provider:%s Decision.Provider:%s BYOK.Protocol:%s BYOK.Endpoint:%s BYOK.Model:%s BYOK.APIKeyEnv:%s Cloud.BaseURL:%s}",
		c.LLM.Provider, c.Decision.Provider, c.BYOK.Protocol, c.BYOK.Endpoint, c.BYOK.Model, c.BYOK.APIKeyEnv, c.Cloud.BaseURL,
	)
}

// Deps are the runtime dependencies Build needs beyond Config: things that
// can't come from a config file (secrets, endpoints, the product's own
// deterministic deciders).
type Deps struct {
	// Product identifies the consuming product for cloud metering/routing.
	Product string
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
}

// Providers is what Build assembles: the single LLM provider to answer chat,
// and the ordered decision-provider chain (feed straight into
// decision.Chain{Providers: providers.Decision}).
type Providers struct {
	LLM      ai.LLMProvider
	Decision []decision.Provider
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
	cfg.ApplyEnv(deps.Getenv, deps.EnvPrefix)
	fillDefaults(&cfg)

	switch cfg.Decision.Provider {
	case "", "auto", "cloud", "disabled":
	default:
		return Providers{}, fmt.Errorf("aiconfig: unknown decision.provider %q", cfg.Decision.Provider)
	}

	var out Providers
	var cloudClient *cloud.Client
	haveCloudToken := deps.CloudToken != nil
	cloudBaseURL := cfg.Cloud.BaseURL
	if cloudBaseURL == "" {
		cloudBaseURL = deps.CloudBaseURL
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
		cloudClient = cloud.New(cloud.Config{
			BaseURL:    cloudBaseURL,
			Product:    deps.Product,
			Token:      deps.CloudToken,
			HTTPClient: deps.HTTPClient,
		})
		return cloudClient, nil
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
		// newCloudClient can only fail when !haveCloudToken or
		// cloudBaseURL=="", both already false in this case, so its error
		// is unreachable here and deliberately ignored rather than checked
		// (simplified per coverage review: an always-nil error branch is
		// dead code, not a real failure path worth a seam).
		c, _ := newCloudClient()
		out.Decision = append(out.Decision, c.Decider())
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
