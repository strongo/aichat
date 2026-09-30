// Package tui holds the small set of messages shared across the
// strongo/aichat Bubble Tea chat kit's sub-packages (tui/stream,
// tui/chatshell) so a leaf component can ask the shell to do something
// (e.g. pin an entity to the sidebar) without importing the shell itself.
// The rendering components (transcript, sidebar, grid block) live in
// github.com/tuigoff/tuigoff; tui/chatshell composes them.
package tui
