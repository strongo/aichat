// Package chatshell composes tui/transcript, tui/sidebar, tui/focus and
// tui/stream into the reusable chat screen every aichat product runs:
// history + composer + sidebar + top/status bars, with a split pane when the
// terminal is wide enough (>= 104 columns, matching DataTug's
// splitEnabled). Products plug in with a Handler and, optionally, an
// OnSidebarChange-style observer; see New and Model's methods.
//
// Keybindings: Enter sends (Shift+Enter inserts a newline); "/" at the start
// of the input opens the slash-command menu (↑↓ choose, Enter insert, Esc
// close); Shift+Up/Down move focus between transcript stops and back to the
// input (see tui/focus); Shift+Right/Left move focus into/out of the
// sidebar; F6 toggles sidebar visibility; Ctrl+Left/Right resize the split;
// Esc always returns focus to the input.
package chatshell
