//go:build unit

package handler

import (
	"context"
	"errors"
	"testing"

	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type videoReceiptHandlerRepo struct {
	*quotedHandlerRepository
	reads int
	err   error
}

func (r *videoReceiptHandlerRepo) FindVideoChargeReceipt(_ context.Context, owner service.VideoAttemptOwner, task string) (*service.VideoChargeReceipt, error) {
	r.reads++
	if r.err != nil {
		return nil, r.err
	}
	if owner.UserID != 10 || owner.APIKeyID != 20 || task != "atlascloud:task-atlas" {
		return nil, service.ErrVideoResultNotFound
	}
	return &service.VideoChargeReceipt{RequestID: "grok-video:" + task, Amount: "3.2500000000", Currency: "USD", UsageLogID: 42}, nil
}

func TestVideoReceiptHandlerNeverPollsOrBills(t *testing.T) {
	for _, scenario := range []string{"owned", "foreign key", "foreign user", "missing", "storage error", "invalid task", "unauthenticated"} {
		t.Run(scenario, func(t *testing.T) {
			repo := &videoReceiptHandlerRepo{quotedHandlerRepository: &quotedHandlerRepository{}}
			h, slots, _, upstream := newGrokMediaSlotHandlerWithBilling(t, false, false, repo, service.PlatformOpenAI, "https://api.atlascloud.ai")
			c, w := atlasVideoContext(t, false, false)
			key, _ := middleware.GetAPIKeyFromContext(c)
			switch scenario {
			case "foreign key":
				key.ID = 99
			case "foreign user":
				key.UserID = 99
			case "missing":
				repo.err = service.ErrVideoResultNotFound
			case "storage error":
				repo.err = errors.New("private database connection string")
			case "invalid task":
				c.Params = gin.Params{{Key: "request_id", Value: "../other"}}
			case "unauthenticated":
				c.Set(string(middleware.ContextKeyAPIKey), nil)
			}
			h.VideoChargeReceipt(c)
			switch scenario {
			case "owned":
				require.Equal(t, 200, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), `"amount":"3.2500000000"`)
			case "storage error":
				require.Equal(t, 503, w.Code, w.Body.String())
				require.NotContains(t, w.Body.String(), "database")
			case "unauthenticated":
				require.Equal(t, 401, w.Code, w.Body.String())
				require.Zero(t, repo.reads)
			default:
				require.Equal(t, 404, w.Code, w.Body.String())
				require.NotContains(t, w.Body.String(), "3.2500000000")
			}
			require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
			require.Zero(t, upstream.calls)
			require.Zero(t, repo.reserves)
			require.Zero(t, repo.captures)
			slots.assertReleased(t)
		})
	}
}
