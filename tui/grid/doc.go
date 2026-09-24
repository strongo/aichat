// Package grid is a product-neutral result-grid transcript block: a
// bubble-table-backed table with sort, per-cell column selection, a
// scrollbar, style presets, and slots for a product's own secondary views
// (charts, a current-row card, raw responses, ...) and split-pane layout.
// Ported and generalised from DataTug chat's GridModel/gridState
// (datatug-cli/pkg/chat/grid.go, ui.go, recordset_ui.go, recordset_views.go,
// table_style.go). DataTug and Sneat Chat use this one grid for
// tabular/contact data — not two competing ones; DataTug's gridState is a
// thin wrapper embedding a *grid.Model.
//
// # Keybindings
//
// ↑↓/k/j move the row (a product's own WithKeyHandler is checked first and
// can still claim "j"/"down" for its own use, e.g. DataTug's join-candidate
// navigation, by handling it before the grid's default runs); ←→/h/l
// select a column, auto-scrolling it into view (SelectedColumn); digit keys
// switch views ("1" is always the table, "2".. select a registered
// ExtraView in order); Tab toggles focus between the table and a split
// secondary view (ToggleSecondaryFocusIfSplit); s sorts (toggling
// ascending/descending) by the selected column; Enter emits RowActivatedMsg;
// + emits tui.AddToSidebarMsg for the highlighted row's Ref; / opens
// bubble-table's built-in filter. While the filter input is focused,
// CapturesEsc reports true so a surrounding chatshell lets Esc clear/blur
// the filter before doing anything else.
//
// A product's WithKeyHandler hook is checked first, for every key the
// filter isn't consuming, and can claim any of the above (e.g. DataTug's
// Enter opens a cell-detail dialog instead of emitting RowActivatedMsg, and
// c/r/a/d/b/B/e/q/space are entirely DataTug's own workspace actions with no
// generic-grid meaning at all).
//
// # Rows and values
//
// Row.Values is positional (aligned with the Columns slice the Row was built
// against), not a map keyed by column name, so two columns sharing a name
// (e.g. `SELECT a.id, b.id`) each keep their own value. Use grid.Absent for a
// column with no value at all for a row (a sparse selection), which renders
// differently from an explicit nil ("NULL"). A Row.Values entry may be a raw
// Go value (formatted by FormatValue) or a product's own pre-formatted
// display string (e.g. DataTug's date-only formatting) — Sort and the table
// cells use whichever was supplied. Row.Key, when set to a stable identifier
// (e.g. the row's original/source index), survives Sort; IndexForKey finds
// it again after a sort or a data refresh.
//
// # Views
//
// There is no fixed Card/Inspector view: the built-in table is always view
// 0; everything else is a product-registered ExtraView (WithExtraViews /
// SetExtraViews), in whatever order the product wants. CardView and
// InspectorView are ready-made ExtraView constructors — a formatted vertical
// field list and a raw-Go-value dump of the highlighted row, respectively —
// for a product that wants one (DataTug registers CardView as its "Current
// row" view, third in its own Table/Charts/Current row/Raw/Headers order).
//
// A product registers its own secondary views (DataTug's Charts, Raw
// response, Headers) with WithExtraViews, and its split-pane policy — table
// and the active secondary view side by side when there's room, generalising
// DataTug's chooseRecordsetLayout — with WithSplitLayout (Model.NaturalWidth
// gives the LayoutFunc the table's unclipped content width to compare
// against the pane's total width). Large results are capped to
// DefaultMaxVisibleRows (or WithMaxVisibleRows's value) per page so a
// 1000-row result never renders fully into a scrolling transcript.
//
// # Style
//
// WithStyle/SetStyle pick a Style preset (StyleLines, StyleSoft,
// StyleMinimal are built in); ParseStyle recovers one by name, e.g. from a
// persisted session. WithFooterHook lets a product append its own text
// (e.g. a save-status badge) to the grid's built-in stats footer (row/column
// range, sort indicator).
//
// # Adoption
//
// DataTug adopts it by mapping a secureread.Result to Columns/Rows:
//
//	cols := make([]grid.Column, len(result.Columns))
//	for i, name := range result.Columns {
//		cols[i] = grid.Column{Name: name, Numeric: columnIsNumeric(result.Rows, name)}
//	}
//	rows := make([]grid.Row, len(result.Rows))
//	for i, row := range result.Rows {
//		values := make([]any, len(result.Columns))
//		for c, name := range result.Columns {
//			if v, ok := row.Data[name]; ok {
//				values[c] = v
//			} else {
//				values[c] = grid.Absent
//			}
//		}
//		rows[i] = grid.Row{Key: strconv.Itoa(i), Values: values}
//	}
//	m := grid.New(cols, rows, grid.WithTitle(title),
//		grid.WithExtraViews(chartsView, grid.CardView("Current row"), rawView, headersView),
//		grid.WithSplitLayout(chooseRecordsetLayout),
//		grid.WithKeyHandler(dataTugGridActions))
package grid
