package durable

import "math"

// Scan positions of the built-in storages (pi storage/scan.ts).

// ScanStart is where a built-in storage scan starts: its order, and the last
// ID a previous page returned (HasAfter false on the first page).
type ScanStart struct {
	Order    ScanOrder
	After    ID
	HasAfter bool
}

// ResolveScanStart resolves a scan's order and position. A cursor continues
// in the order it was created with, whether the query repeats that order or
// omits it; a different order fails. A cursor without an order, written
// before scans had one, continues in the scan's default order.
func ResolveScanStart(requested ScanOrder, cursor Cursor, fallback ScanOrder) (ScanStart, error) {
	if requested != "" && requested != Ascending && requested != Descending {
		return ScanStart{}, NewStorageRequestError("Invalid scan order: %s", requested)
	}
	if cursor == nil {
		order := requested
		if order == "" {
			order = fallback
		}
		return ScanStart{Order: order}, nil
	}
	after, ok := cursorNumber(cursor["after"])
	if !ok {
		return ScanStart{}, NewStorageRequestError("Invalid storage cursor")
	}
	order := fallback
	if stored, present := cursor["order"]; present {
		s, ok := stored.(string)
		if !ok || (ScanOrder(s) != Ascending && ScanOrder(s) != Descending) {
			return ScanStart{}, NewStorageRequestError("Invalid storage cursor")
		}
		order = ScanOrder(s)
	}
	if requested != "" && requested != order {
		return ScanStart{}, NewStorageRequestError("The cursor continues a %s scan; the query asks for %s", order, requested)
	}
	return ScanStart{Order: order, After: after, HasAfter: true}, nil
}

// cursorNumber reads a cursor's safe-integer number, whether it is still a Go
// integer or came back through JSON as a float64.
func cursorNumber(v any) (ID, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return ID(n), true
	case float64:
		if n != math.Trunc(n) || math.Abs(n) > 1<<53-1 {
			return 0, false
		}
		return ID(n), true
	}
	return 0, false
}

// NextCursor is the continuation of a scan whose last returned item has id.
func NextCursor(id ID, order ScanOrder) Cursor {
	return Cursor{"after": id, "order": string(order)}
}
