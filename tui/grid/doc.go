// Package grid is a product-neutral result-grid transcript block: a
// bubble-table-backed table with sort, filter, a current-row card view and a
// raw inspector view, extracted and generalised from DataTug chat's
// GridModel/gridState (datatug-cli/pkg/chat/grid.go, recordset_ui.go,
// recordset_views.go, table_style.go).
//
// Keybindings (table view): ↑↓/j/k move row, ←→/h/l scroll columns,
// 1/2/3 switch view (Table/Card/Inspector), Tab cycles the sort target
// column, s sorts (toggling asc/desc) by it, Enter emits RowActivatedMsg,
// + emits tui.AddToSidebarMsg for the highlighted row's Ref, / opens
// bubble-table's built-in filter.
//
// DataTug adopts it by mapping a secureread.Result to Columns/Rows:
//
//	cols := make([]grid.Column, len(result.Columns))
//	for i, name := range result.Columns {
//		cols[i] = grid.Column{Name: name, Numeric: columnIsNumeric(result.Rows, name)}
//	}
//	rows := make([]grid.Row, len(result.Rows))
//	for i, row := range result.Rows {
//		values := make(map[string]any, len(result.Columns))
//		for _, name := range result.Columns {
//			if v, ok := row.Data[name]; ok {
//				values[name] = v
//			}
//		}
//		rows[i] = grid.Row{Key: strconv.Itoa(i), Values: values}
//	}
//	m := grid.New(cols, rows, grid.WithTitle(title))
package grid
