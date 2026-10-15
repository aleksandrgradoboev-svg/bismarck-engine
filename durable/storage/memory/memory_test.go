package memory_test

import (
	"testing"

	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/durabletest"
	"github.com/aleksandrgradoboev-svg/bismarck-engine/durable/storage/memory"
)

// pi test/memory-storage.test.ts: the in-memory storage passes the storage
// conformance suite.
func TestMemoryStorageConformance(t *testing.T) {
	durabletest.TestStorage(t, func(*testing.T) durable.Storage { return memory.New() })
}
