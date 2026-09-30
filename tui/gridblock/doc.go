// Package gridblock adapts the product-neutral strongo-tui grid.Model to
// aichat's transcript.Block contract, so a result grid can be a chat
// transcript entry. It is the only place aichat and strongo-tui's grid meet:
// grid.Model knows nothing about transcript or session.EntityRef.
package gridblock
