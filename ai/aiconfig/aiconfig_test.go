package aiconfig

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/strongo/aichat/ai/anthropic"
	"github.com/strongo/aichat/ai/cloud"
	"github.com/strongo/aichat/ai/decision"
	"github.com/strongo/aichat/ai/openaicompat"
)

func TestLoad_MissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.Provider != "cloud" || cfg.Decision.Provider != "auto" || cfg.Cloud.BaseURL == "" {
		t.Errorf("cfg = %+v, want defaults", cfg)
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

func TestApplyEnv(t *testing.T) {
	cfg := defaults()
	env := map[string]string{
		EnvLLMProvider:   "byok",
		EnvBYOKProtocol:  "openai-compatible",
		EnvBYOKEndpoint:  "https://my-endpoint/v1",
		EnvBYOKModel:     "gpt-5",
		EnvBYOKAPIKeyEnv: "MY_KEY",
		EnvCloudBaseURL:  "https://cloud.example.com/",
	}
	cfg.ApplyEnv(func(k string) string { return env[k] })
	if cfg.LLM.Provider != "byok" || cfg.BYOK.Endpoint != "https://my-endpoint/v1" || cfg.BYOK.Model != "gpt-5" {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.Cloud.BaseURL != "https://cloud.example.com/" {
		t.Errorf("cfg.Cloud = %+v", cfg.Cloud)
	}
}

func TestApplyEnv_NilGetenvNoop(t *testing.T) {
	cfg := defaults()
	cfg.ApplyEnv(nil) // must not panic
	if cfg.LLM.Provider != "cloud" {
		t.Errorf("cfg mutated unexpectedly: %+v", cfg)
	}
}

func TestConfig_StringNeverLeaksKeyValue(t *testing.T) {
	cfg := defaults()
	cfg.BYOK.APIKeyEnv = "MY_SECRET_ENV_VAR"
	s := cfg.String()
	if !contains(s, "MY_SECRET_ENV_VAR") {
		t.Error("String() should mention the env var NAME")
	}
	// The var name is fine to show; what must never appear is an actual key
	// value, which Config never stores in the first place -- pin that here.
	if contains(s, "sk-") {
		t.Errorf("String() looks like it leaked a key value: %s", s)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func tokenFunc(tok string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return tok, nil }
}

func TestBuild_CloudLLMAndDecision(t *testing.T) {
	cfg := defaults()
	providers, err := Build(cfg, Deps{Product: "sneat", CloudToken: tokenFunc("t")})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := providers.LLM.(*cloud.Client); !ok {
		t.Fatalf("LLM = %T, want *cloud.Client", providers.LLM)
	}
	if len(providers.Decision) != 1 {
		t.Fatalf("Decision = %+v, want 1 cloud decision provider", providers.Decision)
	}
	if _, ok := providers.Decision[0].(*cloud.Client); !ok {
		t.Fatalf("Decision[0] = %T, want *cloud.Client", providers.Decision[0])
	}
}

func TestBuild_CloudLLMWithoutTokenErrors(t *testing.T) {
	cfg := defaults()
	_, err := Build(cfg, Deps{Product: "sneat"})
	if err == nil {
		t.Fatal("expected error when cloud provider requested without a token source")
	}
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
	providers, err := Build(cfg, Deps{CloudToken: tokenFunc("t")})
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

func TestBuild_BYOKMissingEndpointErrors(t *testing.T) {
	cfg := defaults()
	cfg.LLM.Provider = "byok"
	_, err := Build(cfg, Deps{})
	if err == nil {
		t.Fatal("expected error for missing byok.endpoint")
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

func TestBuild_ExtraDecisionRunsBeforeCloud(t *testing.T) {
	cfg := defaults()
	extra := fakeDecider{name: "product-rules"}
	providers, err := Build(cfg, Deps{CloudToken: tokenFunc("t"), ExtraDecision: []decision.Provider{extra}})
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
	providers, err := Build(cfg, Deps{CloudToken: tokenFunc("t"), DisableCloudDecision: true})
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
	providers, err := Build(cfg, Deps{CloudToken: tokenFunc("t")})
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
	cfg.BYOK = BYOK{Endpoint: "https://x"}
	providers, err := Build(cfg, Deps{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(providers.Decision) != 0 {
		t.Fatalf("Decision = %+v, want empty: auto decision needs a cloud token that isn't present", providers.Decision)
	}
}

type fakeDecider struct{ name string }

func (f fakeDecider) Name() string { return f.name }
func (f fakeDecider) Decide(ctx context.Context, req decision.Request) (decision.Decision, bool, error) {
	return decision.Decision{}, false, nil
}
