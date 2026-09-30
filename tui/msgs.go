package tui

import "github.com/strongo/aichat/ai/session"

// AddToSidebarMsg requests that ref be pinned to the working-context sidebar.
// A product control may emit it directly (tui/chatshell also pins on a
// grid's own PinRowMsg). tui/chatshell handles it by adding ref to the
// sidebar and, when the product's Handler implements OnSidebarChange,
// notifying it.
type AddToSidebarMsg struct {
	Ref session.EntityRef
}
