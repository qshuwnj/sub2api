package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

const replaceGroupPricingSQL = `UPDATE groups
SET model_pricing = $1::json
WHERE id = $2 AND updated_at = $3 AND deleted_at IS NULL`

// singleChangedGroupPrice compares wire prices without serialization so metadata
// saves can skip the tariff document. Any pricing change uses the full writer.
func singleChangedGroupPrice(previous, next []service.ChannelModelPricing) (index int, unchanged bool) {
	if len(previous) != len(next) || (previous == nil) != (next == nil) {
		return -1, false
	}
	index = -1
	for i := range next {
		if previous[i].EqualForPersistence(next[i]) {
			continue
		}
		if index >= 0 {
			return -1, false
		}
		index = i
	}
	return index, index < 0
}

// UpdateWithModelPricingSnapshot skips unchanged pricing and atomically replaces
// changed documents as JSON text. jsonb_set still expands the entire old tariff
// tree; replacing opaque JSON text avoids that allocation on large tariffs.
func (r *groupRepository) UpdateWithModelPricingSnapshot(ctx context.Context, groupIn *service.Group, previous []service.ChannelModelPricing) error {
	_, unchanged := singleChangedGroupPrice(previous, groupIn.ModelPricing)
	if unchanged {
		return r.update(ctx, groupIn, false)
	}
	if groupIn.UpdatedAt.IsZero() || r.sql == nil {
		return r.Update(ctx, groupIn)
	}
	pricing, err := json.Marshal(groupIn.ModelPricing)
	if err != nil {
		return fmt.Errorf("marshal group model pricing: %w", err)
	}
	tx, err := r.client.Tx(ctx)
	if errors.Is(err, dbent.ErrTxStarted) {
		// A caller-owned transaction keeps the existing single-write contract.
		return r.Update(ctx, groupIn)
	}
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, replaceGroupPricingSQL,
		string(pricing), groupIn.ID, groupIn.UpdatedAt)
	if err != nil {
		return fmt.Errorf("update group model pricing: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check group model pricing update: %w", err)
	}
	if affected != 1 {
		return service.ErrGroupUpdateConflict
	}
	staged := *groupIn
	// The legacy updater treats outbox errors as best-effort. This transaction
	// must propagate them, so enqueue explicitly after the metadata update.
	scoped := newGroupRepositoryWithSQL(tx.Client(), nil)
	if err := scoped.update(ctx, &staged, false); err != nil {
		return err
	}
	if err := enqueueSchedulerOutbox(ctx, tx, service.SchedulerOutboxEventGroupChanged, nil, &groupIn.ID, nil); err != nil {
		return fmt.Errorf("enqueue group pricing update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	groupIn.UpdatedAt = staged.UpdatedAt
	return nil
}
