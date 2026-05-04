package nodemgr

import (
	"context"
	"fmt"

	"nimbus/internal/db"
)

// SetTags replaces the entire tag set for a node. Empty slice clears tags.
// Tags are stored as a CSV string in db.Node.Tags; the SPA receives them
// as []string via the View.
//
// Tag content rules:
//   - leading/trailing whitespace trimmed
//   - empty entries dropped
//   - duplicates allowed (caller's responsibility to dedupe — keeps the
//     storage close to what the operator typed)
//
// No semantic validation here — tags are operator-defined free text. The
// future workload-aware scheduler is the consumer that will enforce shapes
// (e.g. "gpu" must match an actual GPU host).
func (s *Service) SetTags(ctx context.Context, name string, tags []string) (*db.Node, error) {
	if _, err := s.loadOrCreate(ctx, name); err != nil {
		return nil, fmt.Errorf("load node: %w", err)
	}
	if err := s.db.WithContext(ctx).Model(&db.Node{}).
		Where("name = ?", name).
		Update("tags", joinTags(tags)).Error; err != nil {
		return nil, fmt.Errorf("set tags: %w", err)
	}
	return s.loadOrCreate(ctx, name)
}
