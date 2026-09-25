// Copyright 2026 Sneat.app

package cloudproto

import "github.com/strongo/aichat/ai"

// DetectionStep and ActionExecution are client-observed facts.
type DetectionStep struct {
	Method     string   `json:"method"`
	Detector   string   `json:"detector,omitempty"`
	Version    string   `json:"version,omitempty"`
	Result     string   `json:"result,omitempty"`
	Actions    []string `json:"actions,omitempty"`
	Domains    []string `json:"domains,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"`
}

type ActionExecution struct {
	Action string `json:"action"`
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
}

// InteractionReport records an observable user turn, including turns handled
// entirely by deterministic client logic. User identity is never supplied.
type InteractionReport struct {
	InteractionID      string            `json:"interactionId"`
	Product            string            `json:"product"`
	ClientContext      *ai.ClientContext `json:"clientContext,omitempty"`
	UserMessageChars   int               `json:"userMessageChars"`
	UserMessageWords   int               `json:"userMessageWords"`
	UserMessageTokens  *int              `json:"userMessageTokens,omitempty"`
	DetectionSteps     []DetectionStep   `json:"detectionSteps,omitempty"`
	ActionExecutions   []ActionExecution `json:"actionExecutions,omitempty"`
	Status             string            `json:"status"`
	Outcome            string            `json:"outcome,omitempty"`
	RetryOfInteraction string            `json:"retryOfInteractionId,omitempty"`
	WasRegenerated     bool              `json:"wasRegenerated,omitempty"`
	WasCancelled       bool              `json:"wasCancelled,omitempty"`
}
