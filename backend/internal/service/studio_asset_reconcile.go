package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// StudioVideoIntake is private library intent, never an immutable quote input.
// Completed is only written after an owned native task lookup and a committed
// customer receipt are both proven; this flag alone is never billing proof.
type StudioVideoIntake struct {
	Owner                   StudioAssetOwner
	Resource, Model, Prompt string
	Metadata                json.RawMessage
	Completed               bool
	AccountID               int64
}

type StudioAssetHistoryRepository interface {
	ListStudioAssetHistory(context.Context, StudioAssetOwner, int) ([]string, error)
	FindStudioAssetGeneration(context.Context, StudioAssetOwner, string) (*StudioAsset, error)
}

type StudioAssetImageOutputRepository interface {
	FindStudioAssetOutput(context.Context, StudioAssetOwner, string, int) (*StudioAsset, error)
}

type StudioVideoRecoveryRepository interface {
	ListStudioVideoIntakes(context.Context, int) ([]*StudioVideoIntake, error)
}

func (s *StudioAssetService) SetOwnedImageTasks(tasks *ImageTaskService) {
	if s != nil {
		s.imageTasks = tasks
	}
}

type StudioVideoIntakeRepository interface {
	SaveStudioVideoIntake(context.Context, *StudioVideoIntake) error
	GetStudioVideoIntake(context.Context, StudioAssetOwner, string) (*StudioVideoIntake, error)
	ProcessStudioVideoIntakes(context.Context, int) error
}

func ValidStudioGenerationID(id string) bool {
	id = strings.TrimPrefix(strings.TrimPrefix(id, "attempt:"), "task:")
	return ValidVideoAttemptID(id)
}

func ValidStudioVideoResource(resource string) bool {
	return (strings.HasPrefix(resource, "attempt:") || strings.HasPrefix(resource, "task:")) &&
		ValidVideoAttemptID(strings.SplitN(resource, ":", 2)[1])
}

// RecordVideoIntent runs only on authenticated submission/recovery paths. Stable
// attempt identity is independent of quote hash/selection and is never regenerated.
func (s *StudioAssetService) RecordVideoIntent(ctx context.Context, owner StudioAssetOwner, resource, model, prompt string, metadata json.RawMessage, accountIDs ...int64) error {
	if !validStudioOwner(owner) || !ValidStudioVideoResource(resource) {
		return ErrStudioAssetInvalidRequest
	}
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	if !json.Valid(metadata) || !strings.HasPrefix(strings.TrimSpace(string(metadata)), "{") {
		return ErrStudioAssetInvalidRequest
	}
	if s == nil {
		return ErrStudioAssetUnavailable
	}
	r, ok := s.repo.(StudioVideoIntakeRepository)
	if !ok {
		return ErrStudioAssetUnavailable
	}
	intake := &StudioVideoIntake{Owner: owner, Resource: resource, Model: model, Prompt: prompt, Metadata: metadata}
	if len(accountIDs) == 1 && accountIDs[0] > 0 {
		intake.AccountID = accountIDs[0]
	} else if existing, err := r.GetStudioVideoIntake(ctx, owner, resource); err == nil && existing != nil {
		intake.AccountID = existing.AccountID
	} else if err != nil && !errors.Is(err, ErrStudioAssetNotFound) {
		return ErrStudioAssetUnavailable
	}
	if err := r.SaveStudioVideoIntake(ctx, intake); err != nil {
		return ErrStudioAssetUnavailable
	}
	return nil
}

// CollectVideo never downloads or uploads video. Reconciliation verifies the
// durable completed attempt/owned legacy receipt and references video_results.
func (s *StudioAssetService) CollectVideo(ctx context.Context, owner StudioAssetOwner, generationID, resource, model, prompt string, metadata json.RawMessage) error {
	if generationID != resource || !ValidStudioVideoResource(resource) {
		return ErrStudioAssetInvalidRequest
	}
	if err := s.RecordVideoIntent(ctx, owner, resource, model, prompt, metadata); err != nil {
		return err
	}
	result, err := s.Reconcile(ctx, owner, []string{resource})
	if err != nil || result.Unrecoverable != 0 {
		return ErrStudioAssetUnavailable
	}
	return nil
}

func (s *StudioAssetService) processVideoIntakes(ctx context.Context) error {
	if r, ok := s.repo.(StudioVideoIntakeRepository); ok {
		if err := r.ProcessStudioVideoIntakes(ctx, 100); err != nil {
			return ErrStudioAssetUnavailable
		}
	}
	if r, ok := s.repo.(StudioVideoRecoveryRepository); ok && s.recoverVideo != nil {
		intakes, err := r.ListStudioVideoIntakes(ctx, 2)
		if err != nil {
			return ErrStudioAssetUnavailable
		}
		for _, in := range intakes {
			if ctx.Err() != nil {
				return ErrStudioAssetUnavailable
			}
			// Retry is GET-only. A single expired source must not block others.
			lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_ = s.recoverVideo(lookupCtx, in.Owner, in.Resource)
			cancel()
		}
	}
	return nil
}

func studioHistoryResource(id string) string {
	if strings.HasPrefix(id, "task:") || strings.HasPrefix(id, "attempt:") {
		return id
	}
	if strings.HasPrefix(id, "atlascloud:") {
		return "task:" + id
	}
	return "attempt:" + id
}

func (s *StudioAssetService) reconcileOwnedHistory(ctx context.Context, owner StudioAssetOwner, ids []string) (*StudioAssetReconcileResult, error) {
	history, hasHistory := s.repo.(StudioAssetHistoryRepository)
	if len(ids) == 0 {
		if !hasHistory {
			return s.repo.ReconcileStudioAssets(ctx, owner, nil)
		}
		var err error
		ids, err = history.ListStudioAssetHistory(ctx, owner, 100)
		if err != nil {
			return nil, err
		}
	}
	result := &StudioAssetReconcileResult{}
	var videos []string
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		isImage := strings.HasPrefix(id, "imgtask_")
		if isImage {
			queued, saved := s.recoverImageTask(ctx, owner, id)
			switch {
			case queued:
				result.Queued++
			case saved:
				result.AlreadySaved++
			default:
				result.Unrecoverable++
			}
			continue
		}
		resource := studioHistoryResource(id)
		if hasHistory {
			a, err := history.FindStudioAssetGeneration(ctx, owner, resource)
			if err == nil && a != nil {
				if a.Owner != owner || a.DeletedAt != nil {
					result.Unrecoverable++
					continue
				} else if a.Kind != "video" || a.State != "unavailable" &&
					(!strings.HasPrefix(resource, "task:") || strings.HasPrefix(resource, "task:atlascloud:")) {
					result.AlreadySaved++
					continue
				}
			}
			if err != nil && !errors.Is(err, ErrStudioAssetNotFound) {
				return nil, err
			}
		}
		if s.recoverVideo != nil {
			if err := s.recoverVideo(ctx, owner, resource); err != nil {
				result.Unrecoverable++
				continue
			}
		}
		videos = append(videos, id)
	}
	// Discovered IDs are server-derived, at most 100; each explicit repository
	// request still respects the caller's 50-candidate boundary.
	for len(videos) > 0 {
		n := min(len(videos), 50)
		batch, err := s.repo.ReconcileStudioAssets(ctx, owner, videos[:n])
		if err != nil || batch == nil {
			return nil, ErrStudioAssetUnavailable
		}
		result.Queued += batch.Queued
		result.AlreadySaved += batch.AlreadySaved
		result.Unrecoverable += batch.Unrecoverable
		videos = videos[n:]
	}
	return result, nil
}

func (s *StudioAssetService) recoverImageTask(ctx context.Context, owner StudioAssetOwner, id string) (queued, saved bool) {
	if s.imageTasks == nil {
		return false, false
	}
	task, err := s.imageTasks.Get(ctx, ImageTaskOwner{UserID: owner.UserID, APIKeyID: owner.APIKeyID}, id)
	if err != nil || task == nil || task.ID != id || task.Status != ImageTaskStatusCompleted ||
		task.HTTPStatus < 200 || task.HTTPStatus >= 300 || task.ExpiresAt <= time.Now().Unix() {
		return false, false
	}
	// Completion collectors index source-bearing outputs, not empty raw data
	// slots. Reuse the exact parser so recovery cannot invent a second identity
	// for an existing output (especially an authoritative deletion tombstone).
	outputs := studioImageJSONOutputs(task.Result)
	if len(outputs) == 0 {
		return false, false
	}
	// ImageTaskRecord never retained original request metadata. Do not infer it
	// from response model aliases, revised prompts, or browser-supplied data.
	allIndexed, liveIndexed := true, false
	indexed, canFindOutput := s.repo.(StudioAssetImageOutputRepository)
	generatedAt := time.Unix(task.CreatedAt, 0).UTC()
	if task.CompletedAt != nil {
		generatedAt = time.Unix(*task.CompletedAt, 0).UTC()
	}
	for index, output := range outputs {
		if canFindOutput {
			a, err := indexed.FindStudioAssetOutput(ctx, owner, id, index)
			if err == nil {
				if a == nil || a.Owner != owner || a.GenerationID != id || a.OutputIndex != index || a.Kind != "image" {
					return queued, false
				}
				// Existing outputs are authoritative, including terminal state,
				// active leases, ready objects, immutable metadata and tombstones.
				liveIndexed = liveIndexed || a.DeletedAt == nil
				continue
			}
			if !errors.Is(err, ErrStudioAssetNotFound) {
				return queued, false
			}
		}
		if persisted, _ := s.collectImageAt(ctx, owner, id, index, "", "", nil, output, generatedAt); persisted {
			queued = true
		} else {
			allIndexed = false
		}
	}
	return queued, !queued && allIndexed && liveIndexed
}

// CompleteStudioNativeVideo is called only after the existing authenticated,
// account-bound GET lookup. No supplied URL or task ID alone is sufficient.
func (s *OpenAIGatewayService) CompleteStudioNativeVideo(ctx context.Context, owner StudioAssetOwner, resource, source string, accountID int64, completed bool) error {
	if s == nil || s.studioAssets == nil || !validStudioOwner(owner) || !ValidStudioVideoResource(resource) {
		return ErrStudioAssetUnavailable
	}
	r, ok := s.studioAssets.repo.(StudioVideoIntakeRepository)
	if !ok {
		return ErrStudioAssetUnavailable
	}
	in, err := r.GetStudioVideoIntake(ctx, owner, resource)
	if err != nil {
		return err
	}
	if in == nil || in.Owner != owner || in.Resource != resource || !strings.HasPrefix(resource, "task:") {
		return ErrStudioAssetNotFound
	}
	if !completed {
		return nil // Accepted/running tasks retain intent, never an asset.
	}
	if in.AccountID <= 0 || in.AccountID != accountID {
		return ErrVideoAttemptConflict
	}
	video := &VideoResult{UserID: owner.UserID, APIKeyID: owner.APIKeyID,
		Resource: resource, TaskID: strings.TrimPrefix(resource, "task:"), AccountID: accountID, SourceURL: source}
	if err := requireStudioNativeVideoReceipt(ctx, s.usageBillingRepo, video); err != nil {
		return err
	}
	protected := source == "" && s.videoResults != nil && s.videoResults.nativeContent != nil &&
		studioNativeContentTask(&VideoResult{Resource: resource, TaskID: strings.TrimPrefix(resource, "task:")})
	if accountID <= 0 || s.videoResults == nil || !protected && (!validVideoResultURL(source) || videoSignedURLExpired(source, time.Now())) {
		return ErrStudioAssetUnavailable
	}
	in.Completed = true
	if err := r.SaveStudioVideoIntake(ctx, in); err != nil {
		return ErrStudioAssetUnavailable
	}
	return s.QueueVideoResult(ctx, video)
}
