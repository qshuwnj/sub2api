//go:build unit

package handler

import (
	"bytes"
	"context"
	"io"
	"testing"

	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type archivedHandlerRepo struct {
	*quotedHandlerRepository
	service.VideoResultRepository
}

func (r *archivedHandlerRepo) GetVideoResult(_ context.Context, owner service.VideoAttemptOwner, resource string) (*service.VideoResult, error) {
	if owner.UserID != 10 || owner.APIKeyID != 20 || resource != "attempt:owned-attempt" {
		return nil, service.ErrVideoResultNotFound
	}
	return &service.VideoResult{Resource: resource, UserID: 10, APIKeyID: 20, StorageKey: "video-results/owned.mp4"}, nil
}

type archivedHandlerObjects struct{ signs, opens int }

func (*archivedHandlerObjects) Save(context.Context, string, string, []byte) (string, error) {
	panic("playback must not upload")
}
func (*archivedHandlerObjects) SaveVideo(context.Context, string, string, io.ReadSeeker, int64) error {
	panic("playback must not upload")
}
func (s *archivedHandlerObjects) VideoURL(context.Context, string) (string, error) {
	s.signs++
	return "https://r2.example/refreshed.mp4", nil
}
func (s *archivedHandlerObjects) OpenVideo(_ context.Context, _ string, byteRange string) (*service.VideoResultContent, error) {
	s.opens++
	return &service.VideoResultContent{Body: io.NopCloser(bytes.NewReader([]byte{0, 1, 2, 3})),
		MIME: "video/mp4", Size: 4, Status: 206, ContentRange: "bytes 0-3/100"}, nil
}

func TestArchivedVideoHandlerOwnsContentAndNeverGeneratesOrBills(t *testing.T) {
	for _, scenario := range []string{"attempt", "old task", "foreign key", "multiple ranges", "fresh status"} {
		t.Run(scenario, func(t *testing.T) {
			repo := &archivedHandlerRepo{quotedHandlerRepository: &quotedHandlerRepository{attempt: quotedHandlerAttempt(t)}}
			h, slots, _, upstream := newGrokMediaSlotHandlerWithBilling(t, false, false, repo, service.PlatformOpenAI, "https://api.atlascloud.ai")
			objects := &archivedHandlerObjects{}
			u := service.NewImageResultUploader(objects, "images/", 25<<20, nil)
			h.gatewayService.SetVideoResultStorage(func() (*service.ImageResultUploader, bool) { return u, true })
			c, w := atlasVideoContext(t, false, false)
			c.Params = append(c.Params, struct{ Key, Value string }{"attempt_id", "owned-attempt"})
			c.Request.Header.Set("Range", "bytes=0-3")
			if scenario == "foreign key" {
				key, _ := middleware.GetAPIKeyFromContext(c)
				key.ID = 99
			}
			if scenario == "multiple ranges" {
				c.Request.Header.Set("Range", "bytes=0-3,7-10")
			}
			switch scenario {
			case "old task":
				h.AtlasCloudVideoContent(c)
			case "fresh status":
				h.VideoAttempt(c)
			default:
				h.VideoAttemptContent(c)
			}
			switch scenario {
			case "foreign key":
				require.Equal(t, 404, w.Code, w.Body.String())
				require.Zero(t, objects.opens)
				require.Zero(t, objects.signs)
			case "multiple ranges":
				require.Equal(t, 416, w.Code, w.Body.String())
				require.Zero(t, objects.opens)
			case "fresh status":
				require.Equal(t, 200, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), `"archived":true`)
				require.Contains(t, w.Body.String(), `"content_path":"/v1/videos/atlascloud:task-atlas/content"`)
				require.Contains(t, w.Body.String(), "refreshed.mp4")
			default:
				require.Equal(t, 206, w.Code, w.Body.String())
				require.Equal(t, "bytes 0-3/100", w.Header().Get("Content-Range"))
				require.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
				require.Equal(t, []byte{0, 1, 2, 3}, w.Body.Bytes())
			}
			require.Zero(t, upstream.calls)
			require.Zero(t, repo.reserves)
			require.Zero(t, repo.captures)
			slots.assertReleased(t)
		})
	}
}
