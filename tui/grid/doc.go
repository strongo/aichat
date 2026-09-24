// Package grid is a product-neutral result-grid transcript block: a
// bubble-table-backed table with sort, filter, a current-row card view, a
// raw inspector view, and slots for a product's own secondary views and
// split-pane layout, extracted and generalised from DataTug chat's
// GridModel/gridState (datatug-cli/pkg/chat/grid.go, recordset_ui.go,
// recordset_views.go, table_style.go). DataTug and Sneat Chat use this one
// grid for tabular/contact data — not two competing ones.
//
// Keybindings (table view): ↑↓/j/k move row, ←→/h/l scroll columns,
// 1/2/3 switch view (Table/Card/Inspector), 4.. switch to a registered
// ExtraView, Tab cycles the sort target column, s sorts (toggling asc/desc)
// by it, Enter emits RowActivatedMsg, + emits tui.AddToSidebarMsg for the
// highlighted row's Ref, / opens bubble-table's built-in filter. While the
// filter input is focused, CapturesEsc reports true so a surrounding
// chatshell lets Esc clear/blur the filter before doing anything else.
//
// Row.Values is positional (aligned with the Columns slice the Row was built
// against), not a map keyed by column name, so two columns sharing a name
// (e.g. `SELECT a.id, b.id`) each keep their own value. Use grid.Absent for a
// column with no value at all for a row (a sparse selection), which renders
// differently from an explicit nil ("NULL").
//
// A product registers its own secondary views (DataTug's Charts, Raw
// response, Headers) with WithExtraViews, and its split-pane policy — table
// and the active secondary view side by side when there's room, generalising
// DataTug's chooseRecordsetLayout — with WithSplitLayout. Large results are
// capped to DefaultMaxVisibleRows (or WithMaxVisibleRows's value) per page so
// a 1000-row result never renders fully into a scrolling transcript.
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
//	m := grid.New(cols, rows, grid.WithTitle(title))
package grid
