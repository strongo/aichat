// Copyright 2026 Sneat.app

package ai

// ClientContext describes the caller's environment. Every field is an
// untrusted client claim; servers derive user and request identity separately.
// The same shape is used by CLI, web, native, chat and decision clients.
type ClientContext struct {
	InstallationID string       `json:"installationId,omitempty"`
	SessionID      string       `json:"sessionId,omitempty"`
	ConversationID string       `json:"conversationId,omitempty"`
	Feature        string       `json:"feature,omitempty"`
	Client         ClientInfo   `json:"client"`
	Platform       PlatformInfo `json:"platform"`
	Locale         string       `json:"locale,omitempty"`
}

type ClientInfo struct {
	Type    string `json:"type,omitempty"`
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

type PlatformInfo struct {
	OS   string `json:"os,omitempty"`
	Arch string `json:"arch,omitempty"`
}
