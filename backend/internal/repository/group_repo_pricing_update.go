package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

const replaceGroupPricingCardSQL = `UPDATE groups
SET model_pricing = jsonb_set(model_pricing, $1::text[], $2::jsonb, false)
WHERE id = $3 AND updated_at = $4 AND deleted_at IS NULL
  AND CASE WHEN jsonb_typeof(model_pricing) = 'array'
      THEN jsonb_array_length(model_pricing) = $5 ELSE false END`

// singleChangedGroupPrice identifies the narrow, common edit without
// serializing every unchanged tariff. Structural/bulk edits use the full writer.
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

// UpdateWithModelPricingSnapshot preserves untouched pricing, or sends only
// one changed card to PostgreSQL. It does not make JSONB an in-place disk write.
func (r *groupRepository) UpdateWithModelPricingSnapshot(ctx context.Context, groupIn *service.Group, previous []service.ChannelModelPricing) error {
	index, unchanged := singleChangedGroupPrice(previous, groupIn.ModelPricing)
	if unchanged {
		return r.update(ctx, groupIn, false)
	}
	if index < 0 || groupIn.UpdatedAt.IsZero() || r.sql == nil {
		return r.Update(ctx, groupIn)
	}
	card, err := json.Marshal(groupIn.ModelPricing[index])
	if err != nil {
		return fmt.Errorf("marshal group model pricing card: %w", err)
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

	result, err := tx.ExecContext(ctx, replaceGroupPricingCardSQL,
		pq.Array([]string{strconv.Itoa(index)}), string(card),
		groupIn.ID, groupIn.UpdatedAt, len(previous))
	if err != nil {
		return fmt.Errorf("update group model pricing card: %w", err)
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
