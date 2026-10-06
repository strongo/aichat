package ai

import (
	"context"
	"crypto/sha256"
	"iter"
)

// PreparedChatInfo identifies an exact outbound body without exposing its
// contents or credentials. It is not evidence of a monetary bound.
type PreparedChatInfo struct {
	WireRevision     int
	Protocol         string
	Model            string
	BodySHA256       [sha256.Size]byte
	OutputTokenField string
	MaxOutputTokens  int
}

// PreparedChatCall owns an opaque one-use send lease. A value copy of an
// implementation must share that lease with the original.
type PreparedChatCall interface {
	PreparedInfo() PreparedChatInfo
	Stream(context.Context) iter.Seq2[Event, error]
}

// GuardedChatPreparer finalizes a call before a separate monetary admission.
type GuardedChatPreparer interface {
	PrepareGuardedChat(ChatRequest) (PreparedChatCall, error)
}
