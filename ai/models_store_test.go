package ai

import (
	"context"
	"testing"
)

// TestModelsStoreEntryLastModifiedRoundTrip locks that the InMemoryModelsStore
// preserves the lastModified timestamp across Write/Read (upstream 54fad505).
func TestModelsStoreEntryLastModifiedRoundTrip(t *testing.T) {
	store := NewInMemoryModelsStore()
	if err := store.Write(context.Background(), "p", ModelsStoreEntry{LastModified: 1721577600000, CheckedAt: 1721577601000}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := store.Read(context.Background(), "p")
	if err != nil || got == nil {
		t.Fatalf("read: %+v (err %v)", got, err)
	}
	if got.LastModified != 1721577600000 {
		t.Errorf("LastModified = %d, want 1721577600000", got.LastModified)
	}
	if got.CheckedAt != 1721577601000 {
		t.Errorf("CheckedAt = %d, want 1721577601000", got.CheckedAt)
	}
}

// TestModelsStoreEntryEtagRoundTrip locks that the store preserves the ETag
// validator across Write/Read (upstream b1c444d9).
func TestModelsStoreEntryEtagRoundTrip(t *testing.T) {
	store := NewInMemoryModelsStore()
	if err := store.Write(context.Background(), "p", ModelsStoreEntry{Etag: `"abc123"`}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := store.Read(context.Background(), "p")
	if err != nil || got == nil {
		t.Fatalf("read: %+v (err %v)", got, err)
	}
	if got.Etag != `"abc123"` {
		t.Errorf("Etag = %q, want %q", got.Etag, `"abc123"`)
	}
}
