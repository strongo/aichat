// Package sidebar is the right-hand working-context panel shared by every
// aichat chatshell: an ordered list of session.EntityRef, rendered by a
// product-supplied renderer, with a cursor, remove ("x"/Delete), open
// (Enter) and a resizable split width — generalised from DataTug chat's
// F6 workspace dock and Ctrl+←/→ split handling (ui.go splitEnabled/
// resizeChatPane, workspace_ui.go).
//
// Keybindings: ↑↓/j/k move cursor, x/Delete removes (RemoveMsg), Enter opens
// (OpenMsg). F6 visibility and Ctrl+←/→ width are chatshell-level concerns
// (the sidebar only stores/reports width bounds); see tui/chatshell.
package sidebar
