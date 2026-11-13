package repository

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

const videoQuoteModelPricingSQL = `SELECT COALESCE(json_agg(card), '[]'::json)
FROM groups g
CROSS JOIN LATERAL json_array_elements(COALESCE(g.model_pricing::json, '[]'::json)) AS card
WHERE g.id = $1 AND g.updated_at = $3 AND g.deleted_at IS NULL
AND CASE WHEN json_typeof(card->'models') = 'array' THEN
  json_array_length(card->'models') = 0 OR EXISTS (
    SELECT 1 FROM json_array_elements_text(card->'models') AS m(value)
    WHERE lower(btrim(m.value)) = lower($2) OR position('*' in m.value) > 0
      OR m.value !~ '^[!-~]+$'
  )
ELSE true END
HAVING EXISTS (SELECT 1 FROM groups WHERE id = $1 AND updated_at = $3 AND deleted_at IS NULL)`

// GetByIDForVideoQuote reads fresh admission metadata and only the exact native
// card plus potential wildcard/unscoped conflicts. Domain validation keeps its
// existing fail-closed semantics. A timestamp predicate guards concurrent edits.
func (r *groupRepository) GetByIDForVideoQuote(ctx context.Context, id int64, model string) (*service.Group, error) {
	fields := make([]string, 0, len(group.Columns)-1)
	for _, field := range group.Columns {
		if field != group.FieldModelPricing {
			fields = append(fields, field)
		}
	}
	entity, err := r.client.Group.Query().Where(group.IDEQ(id)).Select(fields...).Only(ctx)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrGroupNotFound, nil)
	}
	rows, err := r.sql.QueryContext(ctx, videoQuoteModelPricingSQL, id, model, entity.UpdatedAt)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("video group changed during fresh admission")
	}
	var pricing []byte
	if err := rows.Scan(&pricing); err != nil {
		return nil, err
	}
	var cards []service.ChannelModelPricing
	if err := json.Unmarshal(pricing, &cards); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := groupEntityToService(entity)
	result.ModelPricing = cards
	return result, nil
}
