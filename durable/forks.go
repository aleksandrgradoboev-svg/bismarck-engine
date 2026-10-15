package durable

import (
	"context"
	"fmt"
)

// Document copies of a conversation fork (pi packages/durable/src/session/forks.ts).

const forkScanPageSize = 256

// forkDocumentCopy is one definition-free document copy created with a
// forked conversation.
type forkDocumentCopy struct {
	record DocumentCreate
	source DocumentCopySource
}

// prepareForkDocumentCopies selects every persisted conversation document a
// fork copies: the "asOf" documents as of the fork entry's commit, in the
// conversation that wrote the entry, and the "current" documents of the
// parent as they are now.
func prepareForkDocumentCopies(ctx context.Context, storage Storage, parent ConversationID, at EntryID, child ConversationID) ([]forkDocumentCopy, error) {
	entry, err := storage.VisibleEntry(ctx, parent, at)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, fmt.Errorf("Entry %d is not visible from conversation %d", at, parent)
	}
	var copies []forkDocumentCopy
	copied := map[string]bool{}
	if err := collectForkCopies(ctx, storage, ConversationScope(entry.Entry.ConversationID), entry.CommitSeq, ForkAsOf, child, &copies, copied); err != nil {
		return nil, err
	}
	if err := collectForkCopies(ctx, storage, ConversationScope(parent), Current, ForkCurrent, child, &copies, copied); err != nil {
		return nil, err
	}
	return copies, nil
}

func collectForkCopies(ctx context.Context, storage Storage, scope DocumentScope, at DocumentPoint, policy string, child ConversationID, copies *[]forkDocumentCopy, copied map[string]bool) error {
	var cursor Cursor
	for {
		page, err := storage.ScanDocuments(ctx, DocumentQuery{Scope: scope, At: at}, forkScanPageSize, cursor)
		if err != nil {
			return err
		}
		for _, source := range page.Items {
			if source.Scope.Kind != ScopeConversation || source.Fork != policy {
				continue
			}
			id, err := storage.MintID(ctx)
			if err != nil {
				return err
			}
			record := DocumentCreate{
				ID:      DocumentID(id),
				Kind:    source.Kind,
				Key:     ptr(source.Key),
				Scope:   ConversationScope(child),
				History: source.History,
				Fork:    source.Fork,
			}
			address := addressID(createAddress(record))
			if copied[address] {
				member := record.Kind
				if record.Key != nil {
					member += "/" + *record.Key
				}
				return fmt.Errorf("Fork selects multiple source documents for %s", member)
			}
			copied[address] = true
			*copies = append(*copies, forkDocumentCopy{record: record, source: DocumentCopySource{ID: source.ID, At: at}})
		}
		if page.Next == nil {
			return nil
		}
		cursor = page.Next
	}
}
