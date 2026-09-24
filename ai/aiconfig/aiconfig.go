// Package aiconfig loads and applies the product-level configuration for
// which LLM and decision providers a product wires up: cloud (Sneat Cloud)
// or BYOK (bring your own key, connecting directly to the provider, never
// via the cloud), and which deciders run before/instead of the cloud's
// decision service.
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
	// Provider is "auto" (default: run configured extras, then cloud if
	// available), "cloud" (force cloud decision on), or "disabled".
	Provider string `yaml:"provider" json:"provider"`
}

// BYOK configures a direct, product-owned connection to an LLM provider.
// BYOK never goes through the cloud boundary.
type BYOK struct {
	// Protocol is "openai-compatible" or "anthropic".
	Protocol string `yaml:"protocol" json:"protocol"`
	Endpoint string `yaml:"endpoint" json:"endpoint"`
	Model    string `yaml:"model" json:"model"`
	// APIKeyEnv names the environment variable holding the API key. The key
	// value itself is never stored in Config.
	APIKeyEnv string `yaml:"apiKeyEnv" json:"apiKeyEnv"`
}

// Cloud configures the Sneat Cloud AI boundary.
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

// Environment variable names ApplyEnv reads. Documented here since they are
// the product-facing configuration surface.
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
		Cloud:    Cloud{BaseURL: "https://api.sneat.cloud/v0/"},
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
	if cfg.Cloud.BaseURL == "" {
		cfg.Cloud.BaseURL = "https://api.sneat.cloud/v0/"
	}
}

// ApplyEnv overrides Config fields from environment variables (see the Env*
// constants), using getenv so callers/tests don't need real process env.
func (c *Config) ApplyEnv(getenv func(string) string) {
	if getenv == nil {
		return
	}
	if v := getenv(EnvLLMProvider); v != "" {
		c.LLM.Provider = v
	}
	if v := getenv(EnvDecision); v != "" {
		c.Decision.Provider = v
	}
	if v := getenv(EnvBYOKProtocol); v != "" {
		c.BYOK.Protocol = v
	}
	if v := getenv(EnvBYOKEndpoint); v != "" {
		c.BYOK.Endpoint = v
	}
	if v := getenv(EnvBYOKModel); v != "" {
		c.BYOK.Model = v
	}
	if v := getenv(EnvBYOKAPIKeyEnv); v != "" {
		c.BYOK.APIKeyEnv = v
	}
	if v := getenv(EnvCloudBaseURL); v != "" {
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
// can't come from a config file (secrets, http client, the product's own
// deterministic deciders).
type Deps struct {
	// Product identifies the consuming product for cloud metering/routing.
	Product string
	// CloudToken supplies the bearer token for cloud requests. Required when
	// LLM.Provider=="cloud" or a cloud decision provider is wired up.
	CloudToken func(context.Context) (string, error)
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
func Build(cfg Config, deps Deps) (Providers, error) {
	if deps.Getenv == nil {
		deps.Getenv = os.Getenv
	}
	if deps.HTTPClient == nil {
		deps.HTTPClient = http.DefaultClient
	}

	var out Providers
	var cloudClient *cloud.Client
	haveCloudToken := deps.CloudToken != nil
	newCloudClient := func() (*cloud.Client, error) {
		if cloudClient != nil {
			return cloudClient, nil
		}
		if !haveCloudToken {
			return nil, fmt.Errorf("aiconfig: cloud provider requested but Deps.CloudToken is nil")
		}
		cloudClient = cloud.New(cloud.Config{
			BaseURL:    cfg.Cloud.BaseURL,
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

	wantCloudDecision := !deps.DisableCloudDecision && (cfg.Decision.Provider == "auto" || cfg.Decision.Provider == "cloud") && haveCloudToken
	if cfg.Decision.Provider == "disabled" {
		wantCloudDecision = false
	}
	if wantCloudDecision {
		c, err := newCloudClient()
		if err != nil {
			return Providers{}, err
		}
		out.Decision = append(out.Decision, c)
	}

	return out, nil
}

func buildBYOK(cfg BYOK, deps Deps) (ai.LLMProvider, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("aiconfig: byok.endpoint is required when llm.provider=byok")
	}
	var apiKey string
	if cfg.APIKeyEnv != "" {
		apiKey = deps.Getenv(cfg.APIKeyEnv)
	}
	switch strings.ToLower(cfg.Protocol) {
	case "", "openai-compatible":
		return openaicompat.New(openaicompat.Config{
			BaseURL:    cfg.Endpoint,
			APIKey:     apiKey,
			Model:      cfg.Model,
			HTTPClient: deps.HTTPClient,
		}), nil
	case "anthropic":
		return anthropic.New(anthropic.Config{
			BaseURL:    cfg.Endpoint,
			APIKey:     apiKey,
			Model:      cfg.Model,
			HTTPClient: deps.HTTPClient,
		}), nil
	default:
		return nil, fmt.Errorf("aiconfig: unknown byok.protocol %q", cfg.Protocol)
	}
}
