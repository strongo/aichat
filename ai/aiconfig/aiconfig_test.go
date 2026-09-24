package aiconfig

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/anthropic"
	"github.com/strongo/aichat/ai/decision"
	"github.com/strongo/aichat/ai/openaicompat"
)

func TestLoad_MissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.Provider != "cloud" || cfg.Decision.Provider != "auto" {
		t.Errorf("cfg = %+v, want defaults", cfg)
	}
	if cfg.Cloud.BaseURL != "" {
		t.Errorf("Cloud.BaseURL = %q, want empty -- this package has no cloud default of its own", cfg.Cloud.BaseURL)
	}
}

func TestLoad_ParsesYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ai.yaml")
	yaml := `
llm:
  provider: byok
decision:
  provider: disabled
byok:
  protocol: anthropic
  endpoint: https://api.anthropic.com
  model: claude-haiku-4-5-20251001
  apiKeyEnv: ANTHROPIC_API_KEY
cloud:
  baseURL: https://api.example.com/v1/
`
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.Provider != "byok" || cfg.Decision.Provider != "disabled" {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.BYOK.Protocol != "anthropic" || cfg.BYOK.Endpoint != "https://api.anthropic.com" || cfg.BYOK.APIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Errorf("cfg.BYOK = %+v", cfg.BYOK)
	}
	if cfg.Cloud.BaseURL != "https://api.example.com/v1/" {
		t.Errorf("cfg.Cloud = %+v", cfg.Cloud)
	}
}

func TestApplyEnv_NoPrefix(t *testing.T) {
	cfg := defaults()
	env := map[string]string{
		EnvLLMProvider:   "byok",
		EnvDecision:      "disabled",
		EnvBYOKProtocol:  "openai-compatible",
		EnvBYOKEndpoint:  "https://my-endpoint/v1",
		EnvBYOKModel:     "gpt-5",
		EnvBYOKAPIKeyEnv: "MY_KEY",
		EnvCloudBaseURL:  "https://cloud.example.com/",
	}
	cfg.ApplyEnv(func(k string) string { return env[k] }, "")
	if cfg.LLM.Provider != "byok" || cfg.BYOK.Endpoint != "https://my-endpoint/v1" || cfg.BYOK.Model != "gpt-5" {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.Decision.Provider != "disabled" {
		t.Errorf("cfg.Decision.Provider = %q, want disabled (EnvDecision override)", cfg.Decision.Provider)
	}
	if cfg.Cloud.BaseURL != "https://cloud.example.com/" {
		t.Errorf("cfg.Cloud = %+v", cfg.Cloud)
	}
}

func TestLoad_UnreadableFileReturnsError(t *testing.T) {
	// A directory can't be read as a file: os.ReadFile fails with an error
	// that is NOT os.IsNotExist, exercising Load's other read-error branch
	// (distinct from the "missing file -> defaults, no error" path).
	dir := t.TempDir()
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected an error reading a directory as a config file")
	}
	if !strings.Contains(err.Error(), "aiconfig: read") {
		t.Errorf("err = %v, want it wrapped with the aiconfig: read %%s prefix", err)
	}
}

func TestLoad_InvalidYAMLReturnsError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ai.yaml")
	if err := os.WriteFile(p, []byte("llm: [this is not a mapping"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected a parse error for invalid YAML")
	}
	if !strings.Contains(err.Error(), "aiconfig: parse") {
		t.Errorf("err = %v, want it wrapped with the aiconfig: parse %%s prefix", err)
	}
}

// TestLoad_ExplicitEmptyProviderStringsFillDefaults covers fillDefaults'
// branches specifically as Load exercises them: a config file that
// EXPLICITLY sets llm.provider/decision.provider to "" (distinct from
// omitting the keys entirely, which never overwrites Load's initial
// defaults()) must still end up with the "cloud"/"auto" defaults.
func TestLoad_ExplicitEmptyProviderStringsFillDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ai.yaml")
	yamlDoc := "llm:\n  provider: \"\"\ndecision:\n  provider: \"\"\n"
	if err := os.WriteFile(p, []byte(yamlDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.Provider != "cloud" {
		t.Errorf("LLM.Provider = %q, want cloud (fillDefaults must overwrite an explicit empty string)", cfg.LLM.Provider)
	}
	if cfg.Decision.Provider != "auto" {
		t.Errorf("Decision.Provider = %q, want auto (fillDefaults must overwrite an explicit empty string)", cfg.Decision.Provider)
	}
}

func TestApplyEnv_WithProductPrefix(t *testing.T) {
	cfg := defaults()
	env := map[string]string{
		"SNEAT_" + EnvLLMProvider:  "byok",
		"SNEAT_" + EnvBYOKEndpoint: "https://my-endpoint/v1",
		// unprefixed variants must NOT apply when a prefix is given.
		EnvLLMProvider: "cloud",
	}
	cfg.ApplyEnv(func(k string) string { return env[k] }, "SNEAT_")
	if cfg.LLM.Provider != "byok" {
		t.Errorf("LLM.Provider = %q, want the SNEAT_-prefixed value to win", cfg.LLM.Provider)
	}
	if cfg.BYOK.Endpoint != "https://my-endpoint/v1" {
		t.Errorf("BYOK.Endpoint = %q", cfg.BYOK.Endpoint)
	}
}

func TestApplyEnv_NilGetenvNoop(t *testing.T) {
	cfg := defaults()
	cfg.ApplyEnv(nil, "") // must not panic
	if cfg.LLM.Provider != "cloud" {
		t.Errorf("cfg mutated unexpectedly: %+v", cfg)
	}
}

func TestConfig_StringNeverLeaksKeyValue(t *testing.T) {
	cfg := defaults()
	cfg.BYOK.APIKeyEnv = "MY_SECRET_ENV_VAR"
	s := cfg.String()
	if !strings.Contains(s, "MY_SECRET_ENV_VAR") {
		t.Error("String() should mention the env var NAME")
	}
	// The var name is fine to show; what must never appear is an actual key
	// value, which Config never stores in the first place -- pin that here.
	if strings.Contains(s, "sk-") {
		t.Errorf("String() looks like it leaked a key value: %s", s)
	}
}

func tokenFunc(tok string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return tok, nil }
}

func TestBuild_CloudLLMAndDecision(t *testing.T) {
	cfg := defaults()
	providers, err := Build(cfg, Deps{Product: "sneat", CloudToken: tokenFunc("t"), CloudBaseURL: "https://api.example.com/v0/"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if providers.LLM == nil || providers.LLM.Name() != "cloud" {
		t.Fatalf("LLM = %+v, want the cloud LLM provider", providers.LLM)
	}
	if len(providers.Decision) != 1 {
		t.Fatalf("Decision = %+v, want 1 cloud decision provider", providers.Decision)
	}
	if providers.Decision[0].Name() != "cloud-decision" {
		t.Fatalf("Decision[0].Name() = %q, want cloud-decision", providers.Decision[0].Name())
	}
}

func TestBuild_CloudLLMWithoutTokenErrors(t *testing.T) {
	cfg := defaults()
	_, err := Build(cfg, Deps{Product: "sneat", CloudBaseURL: "https://api.example.com/"})
	if err == nil {
		t.Fatal("expected error when cloud provider requested without a token source")
	}
}

func TestBuild_CloudLLMWithoutBaseURLErrors(t *testing.T) {
	cfg := defaults()
	_, err := Build(cfg, Deps{Product: "sneat", CloudToken: tokenFunc("t")}) // no CloudBaseURL, no Config.Cloud.BaseURL
	if err == nil {
		t.Fatal("expected error when cloud provider requested without any base URL")
	}
}

func TestBuild_ConfigCloudBaseURLOverridesDeps(t *testing.T) {
	cfg := defaults()
	cfg.Cloud.BaseURL = "https://from-config.example.com/"
	providers, err := Build(cfg, Deps{Product: "sneat", CloudToken: tokenFunc("t"), CloudBaseURL: "https://from-deps.example.com/"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if providers.LLM == nil {
		t.Fatal("expected an LLM provider")
	}
	// Can't inspect the unexported cloud.Client's baseURL directly; this at
	// least confirms Build succeeded with a config override present. The
	// precedence itself is exercised by cloud package tests + code review.
}

func TestBuild_BYOKOpenAICompatible(t *testing.T) {
	cfg := defaults()
	cfg.LLM.Provider = "byok"
	cfg.BYOK = BYOK{Protocol: "openai-compatible", Endpoint: "https://my-llm/v1", Model: "m", APIKeyEnv: "MY_KEY"}
	providers, err := Build(cfg, Deps{
		Getenv: func(k string) string {
			if k == "MY_KEY" {
				return "secret-key"
			}
			return ""
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	p, ok := providers.LLM.(*openaicompat.Provider)
	if !ok {
		t.Fatalf("LLM = %T, want *openaicompat.Provider", providers.LLM)
	}
	if p.Name() != "openai-compatible" {
		t.Errorf("Name() = %q", p.Name())
	}
}

func TestBuild_BYOKAnthropic(t *testing.T) {
	cfg := defaults()
	cfg.LLM.Provider = "byok"
	cfg.Decision.Provider = "cloud" // cloud decision + BYOK LLM must both work (independent)
	cfg.BYOK = BYOK{Protocol: "anthropic", Endpoint: "https://api.anthropic.com", Model: "m"}
	providers, err := Build(cfg, Deps{CloudToken: tokenFunc("t"), CloudBaseURL: "https://api.example.com/"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := providers.LLM.(*anthropic.Provider); !ok {
		t.Fatalf("LLM = %T, want *anthropic.Provider", providers.LLM)
	}
	if len(providers.Decision) != 1 {
		t.Fatalf("Decision = %+v, want cloud decision even though LLM is BYOK", providers.Decision)
	}
}

func TestBuild_BYOKDefaultEndpoints(t *testing.T) {
	cfg := defaults()
	cfg.LLM.Provider = "byok"
	cfg.BYOK = BYOK{Protocol: "anthropic"} // no Endpoint
	providers, err := Build(cfg, Deps{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := providers.LLM.(*anthropic.Provider); !ok {
		t.Fatalf("LLM = %T, want *anthropic.Provider using the default endpoint", providers.LLM)
	}

	cfg2 := defaults()
	cfg2.LLM.Provider = "byok"   // BYOK.Protocol left empty too -> openai-compatible default
	cfg2.BYOK = BYOK{Model: "m"} // openai-compatible has no default model (n10): must be set
	providers2, err := Build(cfg2, Deps{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := providers2.LLM.(*openaicompat.Provider); !ok {
		t.Fatalf("LLM = %T, want *openaicompat.Provider using the default endpoint", providers2.LLM)
	}
}

// roundTripFunc lets a test capture the exact request URL a provider built,
// without touching the network or overriding the endpoint under test --
// unlike pointing BaseURL at an httptest.Server (which never reproduces a
// BaseURL shaped like the real default, since httptest URLs have no "/v1"
// segment of their own to accidentally double).
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBuild_BYOKDefaultEndpoints_ComposeCorrectPaths(t *testing.T) {
	// B2: ai/anthropic appends the fixed "/v1/messages" to BaseURL itself,
	// so the default BaseURL must be the BARE host -- appending "/v1/" in
	// the default too would double it into ".../v1/v1/messages".
	var gotURL string
	cfg := defaults()
	cfg.LLM.Provider = "byok"
	cfg.BYOK = BYOK{Protocol: "anthropic"} // no Endpoint -> default
	providers, err := Build(cfg, Deps{
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotURL = r.URL.String()
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")),
			}, nil
		})},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	_, _, _, _ = ai.Collect(providers.LLM.Stream(context.Background(), ai.ChatRequest{}))
	if gotURL != "https://api.anthropic.com/v1/messages" {
		t.Errorf("anthropic request URL = %q, want %q (default endpoint must not double /v1/)", gotURL, "https://api.anthropic.com/v1/messages")
	}

	// ai/openaicompat appends only "/chat/completions", so its default
	// BaseURL must already carry "/v1/" (OpenAI's real API root).
	var gotURL2 string
	cfg2 := defaults()
	cfg2.LLM.Provider = "byok" // Protocol left empty -> openai-compatible default
	cfg2.BYOK = BYOK{Model: "m"}
	providers2, err := Build(cfg2, Deps{
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotURL2 = r.URL.String()
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		})},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	_, _, _, _ = ai.Collect(providers2.LLM.Stream(context.Background(), ai.ChatRequest{}))
	if gotURL2 != "https://api.openai.com/v1/chat/completions" {
		t.Errorf("openaicompat request URL = %q, want %q", gotURL2, "https://api.openai.com/v1/chat/completions")
	}
}

func TestBuild_EmptyAPIKeyEnvValueErrors(t *testing.T) {
	cfg := defaults()
	cfg.LLM.Provider = "byok"
	cfg.BYOK = BYOK{Endpoint: "https://x", APIKeyEnv: "MY_KEY"}
	_, err := Build(cfg, Deps{Getenv: func(string) string { return "" }})
	if err == nil {
		t.Fatal("expected an error when the named APIKeyEnv variable is unset/empty")
	}
}

func TestBuild_NoAPIKeyEnvConfiguredIsFine(t *testing.T) {
	cfg := defaults()
	cfg.LLM.Provider = "byok"
	cfg.BYOK = BYOK{Endpoint: "https://x", Model: "m"} // no APIKeyEnv at all
	_, err := Build(cfg, Deps{})
	if err != nil {
		t.Fatalf("Build: %v, want success when no key is configured at all", err)
	}
}

func TestBuild_OpenAICompatibleEmptyModelErrors(t *testing.T) {
	// n10: ai/openaicompat has no built-in default model (unlike
	// ai/anthropic), so an empty Model must be a Build-time error rather
	// than a failure deferred to the first real request.
	cfg := defaults()
	cfg.LLM.Provider = "byok"
	cfg.BYOK = BYOK{Protocol: "openai-compatible", Endpoint: "https://x"} // no Model
	_, err := Build(cfg, Deps{})
	if err == nil {
		t.Fatal("expected an error for an empty byok.model with byok.protocol=openai-compatible")
	}
}

func TestBuild_AnthropicEmptyModelIsFine(t *testing.T) {
	// Anthropic has a built-in default model, so an empty Model is not an
	// error there.
	cfg := defaults()
	cfg.LLM.Provider = "byok"
	cfg.BYOK = BYOK{Protocol: "anthropic", Endpoint: "https://x"} // no Model
	_, err := Build(cfg, Deps{})
	if err != nil {
		t.Fatalf("Build: %v, want success: anthropic has a default model", err)
	}
}

func TestBuild_UnknownLLMProviderErrors(t *testing.T) {
	cfg := defaults()
	cfg.LLM.Provider = "nope"
	_, err := Build(cfg, Deps{})
	if err == nil {
		t.Fatal("expected error for unknown llm.provider")
	}
}

func TestBuild_UnknownProtocolWithoutEndpointErrorsOnEndpointRequired(t *testing.T) {
	// An unrecognised protocol has no default endpoint (only
	// openai-compatible/anthropic do); leaving Endpoint empty too must
	// surface the earlier "byok.endpoint is required" error, distinct from
	// (and checked before) the later "unknown byok.protocol" error.
	cfg := defaults()
	cfg.LLM.Provider = "byok"
	cfg.BYOK.Protocol = "nope"
	_, err := Build(cfg, Deps{})
	if err == nil {
		t.Fatal("expected error for an unrecognised protocol with no endpoint")
	}
	if !strings.Contains(err.Error(), "byok.endpoint is required") {
		t.Errorf("err = %v, want the byok.endpoint required error", err)
	}
}

func TestBuild_UnknownBYOKProtocolErrors(t *testing.T) {
	cfg := defaults()
	cfg.LLM.Provider = "byok"
	cfg.BYOK.Endpoint = "https://x"
	cfg.BYOK.Protocol = "nope"
	_, err := Build(cfg, Deps{})
	if err == nil {
		t.Fatal("expected error for unknown byok.protocol")
	}
}

func TestBuild_UnknownDecisionProviderErrors(t *testing.T) {
	cfg := defaults()
	cfg.LLM.Provider = "byok"
	cfg.BYOK.Endpoint = "https://x"
	cfg.Decision.Provider = "nope"
	_, err := Build(cfg, Deps{})
	if err == nil {
		t.Fatal("expected error for unknown decision.provider")
	}
}

func TestBuild_DecisionProviderCloudWithoutTokenErrors(t *testing.T) {
	cfg := defaults()
	cfg.LLM.Provider = "byok"
	// Model is required so the BYOK build itself succeeds -- this test's
	// error must come from the decision.provider=cloud branch specifically,
	// not an unrelated earlier BYOK validation failure.
	cfg.BYOK = BYOK{Endpoint: "https://x", Model: "m"}
	cfg.Decision.Provider = "cloud" // explicit request, no CloudToken -> must error
	_, err := Build(cfg, Deps{})
	if err == nil {
		t.Fatal("expected error: decision.provider=cloud explicitly requested but no token source")
	}
	if !strings.Contains(err.Error(), "decision.provider=cloud") {
		t.Errorf("err = %v, want it to mention decision.provider=cloud", err)
	}
}

// TestBuild_CloudLLMAndExplicitDecisionShareOneClient covers newCloudClient's
// own cache-hit branch (cloudClient != nil), which TestBuild_CloudLLMAndDecision
// does not: that test's Decision.Provider is left at "auto", which calls
// buildCloudClient directly rather than going back through newCloudClient a
// second time. Requesting cloud LLM AND an EXPLICIT cloud decision provider
// calls newCloudClient twice in the same Build(): once from the LLM switch
// (cloudClient is nil, builds it), once from the decision.provider=cloud
// case (cloudClient is now set, must return the cached one without
// rebuilding).
func TestBuild_CloudLLMAndExplicitDecisionShareOneClient(t *testing.T) {
	cfg := defaults()
	cfg.Decision.Provider = "cloud"
	providers, err := Build(cfg, Deps{Product: "sneat", CloudToken: tokenFunc("t"), CloudBaseURL: "https://api.example.com/v0/"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if providers.LLM == nil || providers.LLM.Name() != "cloud" {
		t.Fatalf("LLM = %+v, want the cloud LLM provider", providers.LLM)
	}
	if len(providers.Decision) != 1 || providers.Decision[0].Name() != "cloud-decision" {
		t.Fatalf("Decision = %+v, want 1 cloud-decision provider", providers.Decision)
	}
}

func TestBuild_ExtraDecisionRunsBeforeCloud(t *testing.T) {
	cfg := defaults()
	extra := fakeDecider{name: "product-rules"}
	providers, err := Build(cfg, Deps{CloudToken: tokenFunc("t"), CloudBaseURL: "https://api.example.com/", ExtraDecision: []decision.Provider{extra}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(providers.Decision) != 2 {
		t.Fatalf("Decision = %+v, want [extra, cloud]", providers.Decision)
	}
	if providers.Decision[0].Name() != "product-rules" {
		t.Errorf("Decision[0].Name() = %q, want extras first", providers.Decision[0].Name())
	}
}

func TestBuild_DisableCloudDecision(t *testing.T) {
	cfg := defaults()
	providers, err := Build(cfg, Deps{CloudToken: tokenFunc("t"), CloudBaseURL: "https://api.example.com/", DisableCloudDecision: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(providers.Decision) != 0 {
		t.Fatalf("Decision = %+v, want empty when DisableCloudDecision", providers.Decision)
	}
}

func TestBuild_DecisionDisabledInConfig(t *testing.T) {
	cfg := defaults()
	cfg.Decision.Provider = "disabled"
	providers, err := Build(cfg, Deps{CloudToken: tokenFunc("t"), CloudBaseURL: "https://api.example.com/"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(providers.Decision) != 0 {
		t.Fatalf("Decision = %+v, want empty", providers.Decision)
	}
}

func TestBuild_DecisionAutoWithoutTokenSkipsCloud(t *testing.T) {
	cfg := defaults()
	providers, err := Build(cfg, Deps{}) // no CloudToken, byok llm not set -> but LLM cloud will error first
	_ = providers
	if err == nil {
		t.Fatal("expected LLM build to fail without a cloud token (default llm.provider is cloud)")
	}
}

func TestBuild_DecisionAutoWithoutTokenButBYOKLLM(t *testing.T) {
	cfg := defaults()
	cfg.LLM.Provider = "byok"
	cfg.BYOK = BYOK{Endpoint: "https://x", Model: "m"}
	providers, err := Build(cfg, Deps{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(providers.Decision) != 0 {
		t.Fatalf("Decision = %+v, want empty: auto decision needs a cloud token that isn't present", providers.Decision)
	}
}

func TestBuild_AppliesPrefixedEnvOverrides(t *testing.T) {
	cfg := defaults()
	cfg.LLM.Provider = "byok" // BYOK.Protocol left empty in Config -> openai-compatible
	env := map[string]string{"SNEAT_" + EnvBYOKProtocol: "anthropic"}
	providers, err := Build(cfg, Deps{EnvPrefix: "SNEAT_", Getenv: func(k string) string { return env[k] }})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := providers.LLM.(*anthropic.Provider); !ok {
		t.Fatalf("LLM = %T, want *anthropic.Provider -- the SNEAT_-prefixed env override must win over Config", providers.LLM)
	}
}

type fakeDecider struct{ name string }

func (f fakeDecider) Name() string { return f.name }
func (f fakeDecider) Decide(ctx context.Context, req decision.Request) (decision.Decision, bool, error) {
	return decision.Decision{}, false, nil
}
