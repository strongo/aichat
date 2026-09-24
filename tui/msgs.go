package tui

import "github.com/strongo/aichat/ai/session"

// AddToSidebarMsg requests that ref be pinned to the working-context sidebar.
// Leaf components such as tui/grid emit it (e.g. on "+" over the current
// row); tui/chatshell handles it by adding ref to tui/sidebar and, when the
// product's Handler implements OnSidebarChange, notifying it.
type AddToSidebarMsg struct {
	Ref session.EntityRef
}
