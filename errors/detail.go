package errors

import (
	"fmt"
	"strings"
)

// detailIndent prefixes every line of a Detail block. Two spaces, so a message
// pasted into an issue keeps its shape inside a code fence.
const detailIndent = "  "

// Detail renders err across several lines, with its fields aligned and its fix
// last:
//
//	warren.Run: di.Resolve: no provider for *sql.DB
//
//	  requested by  internal/modules/orders/module.go:14
//	  chain         *OrdersHandler → *OrderRepository → *sql.DB
//
//	  fix: add warren.Provide(NewDB) to internal/platform/module.go
//
// It is the rendering for a terminal or a log entry that can afford the space.
// Single-line callers use err.Error() instead.
//
// An error carrying neither fields nor a fix renders as err.Error(), so Detail
// is always safe to call. Detail(nil) returns "".
func Detail(err error) string {
	if err == nil {
		return ""
	}

	blocks := []string{err.Error()}

	if fields := Fields(err); len(fields) > 0 {
		blocks = append(blocks, fieldBlock(fields))
	}

	if fix := Fix(err); fix != "" {
		blocks = append(blocks, detailIndent+"fix: "+fix)
	}

	return strings.Join(blocks, "\n\n")
}

// fieldBlock renders fields one per line, with the values aligned into a
// column so that a reader scans them vertically.
func fieldBlock(fields []Field) string {
	width := 0

	for _, f := range fields {
		if len(f.Key) > width {
			width = len(f.Key)
		}
	}

	lines := make([]string, 0, len(fields))

	for _, f := range fields {
		lines = append(lines, fmt.Sprintf("%s%-*s%s%v", detailIndent, width, f.Key, detailIndent, f.Value))
	}

	return strings.Join(lines, "\n")
}
