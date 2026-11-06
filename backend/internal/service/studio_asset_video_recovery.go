package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

var errStudioNativeContent = errors.New("owned native video uses authenticated content")

type StudioNativeVideoReceiptRepository interface {
	FindStudioNativeVideoReceipt(context.Context, VideoAttemptOwner, string) (*VideoResult, error)
}

func studioNativeVideoResource(v *VideoResult) bool {
	return v != nil && v.Resource == "task:"+v.TaskID && ValidVideoAttemptID(v.TaskID) &&
		!strings.HasPrefix(v.TaskID, "atlascloud:")
}

// A successful supplier GET is not customer billing proof. This read-only gate
// also protects rows queued by earlier versions before the receipt was committed.
func requireStudioNativeVideoReceipt(ctx context.Context, repo any, v *VideoResult) error {
	if v == nil || !strings.HasPrefix(v.Resource, "task:") || strings.HasPrefix(v.Resource, "task:atlascloud:") {
		return nil
	}
	if !studioNativeVideoResource(v) {
		return ErrVideoResultNotFound
	}
	r, ok := repo.(StudioNativeVideoReceiptRepository)
	if !ok {
		return ErrVideoResultUnavailable
	}
	proof, err := r.FindStudioNativeVideoReceipt(ctx, VideoAttemptOwner{UserID: v.UserID, APIKeyID: v.APIKeyID}, v.TaskID)
	if err != nil {
		return err
	}
	if proof == nil || proof.UserID != v.UserID || proof.APIKeyID != v.APIKeyID ||
		proof.Resource != v.Resource || proof.TaskID != v.TaskID || proof.AccountID <= 0 || proof.AccountID != v.AccountID {
		return ErrVideoResultNotFound
	}
	return nil
}

func studioNativeContentTask(v *VideoResult) bool {
	return v != nil && v.Resource == "task:"+v.TaskID && ValidVideoAttemptID(v.TaskID) &&
		!strings.HasPrefix(v.TaskID, "atlascloud:") && !strings.HasPrefix(v.TaskID, "seedance:")
}

// A pending authenticated-content reference is usable only alongside a durable
// completed intake bound to the same owner and account.
func StudioVideoPendingNativeContent(v *VideoResult, in *StudioVideoIntake) bool {
	return studioNativeContentTask(v) && v.SourceURL == "" && in != nil && in.Completed &&
		in.Owner == (StudioAssetOwner{UserID: v.UserID, APIKeyID: v.APIKeyID}) &&
		in.Resource == v.Resource && in.AccountID > 0 && in.AccountID == v.AccountID
}

func StudioVideoSourceUsable(v *VideoResult) bool {
	if v == nil {
		return false
	}
	if v.StorageKey != "" {
		return v.Size > 0 && v.Size <= 300<<20 && (v.MIME == "video/mp4" || v.MIME == "video/webm")
	}
	return validVideoResultURL(v.SourceURL) && !videoSignedURLExpired(v.SourceURL, time.Now())
}

// All requests here use an existing gateway task lookup, never a submission,
// scheduler, billing/settlement method, guessed route, or synthesized provider ID.
func (s *OpenAIGatewayService) recoverStudioVideoResource(ctx context.Context, owner StudioAssetOwner, resource string) error {
	if s == nil || s.videoResults == nil || !validStudioOwner(owner) || !ValidStudioVideoResource(resource) {
		return ErrVideoResultUnavailable
	}
	videoOwner := VideoAttemptOwner{UserID: owner.UserID, APIKeyID: owner.APIKeyID}
	if task, isTask := strings.CutPrefix(resource, "task:"); isTask {
		if attempt, err := s.FindQuotedVideoTask(ctx, videoOwner, task); err == nil {
			resource = "attempt:" + attempt.ID
		} else if !errors.Is(err, ErrVideoAttemptNotFound) {
			return err
		}
	}
	v, err := s.videoResults.repo.GetVideoResult(ctx, videoOwner, resource)
	if err != nil && !errors.Is(err, ErrVideoResultNotFound) {
		return err
	}
	if v != nil && (v.UserID != owner.UserID || v.APIKeyID != owner.APIKeyID || v.Resource != resource) {
		return ErrVideoResultNotFound
	}
	if v != nil {
		if err := requireStudioNativeVideoReceipt(ctx, s.videoResults.repo, v); err != nil {
			return err
		}
	}
	if StudioVideoSourceUsable(v) {
		return nil // Prefer the one existing object/outbox; never duplicate upload.
	}
	if id, isAttempt := strings.CutPrefix(resource, "attempt:"); isAttempt {
		a, err := s.GetQuotedVideo(ctx, videoOwner, id)
		if err != nil {
			return err
		}
		if a.UserID != owner.UserID || a.APIKeyID != owner.APIKeyID || a.State != VideoAttemptCompleted ||
			a.FinancialState != VideoFinancialCaptured || a.Receipt == nil || len(a.ResultURLs) != 1 {
			return ErrVideoResultNotFound
		}
		_, err = s.refreshCompletedVideo(ctx, a, true)
		return err
	}
	task := strings.TrimPrefix(resource, "task:")
	if strings.HasPrefix(task, "atlascloud:") {
		proof, err := s.videoResults.repo.FindLegacyVideoResult(ctx, videoOwner, task)
		if err != nil {
			return err
		}
		if v == nil {
			v = proof
		} else if v.AccountID != proof.AccountID {
			return ErrVideoAttemptConflict
		}
		source, err := s.refreshStoredVideoSource(ctx, v)
		if err != nil {
			return err
		}
		v.SourceURL = source
		return s.QueueVideoResult(ctx, v)
	}
	r, ok := s.studioAssets.repo.(StudioVideoIntakeRepository)
	if !ok {
		return ErrVideoResultUnavailable
	}
	in, err := r.GetStudioVideoIntake(ctx, owner, resource)
	if err != nil {
		return err
	}
	if in == nil || in.Owner != owner || in.Resource != resource || in.AccountID <= 0 {
		return ErrVideoResultNotFound
	}
	if v == nil {
		v = &VideoResult{Resource: resource, TaskID: task, UserID: owner.UserID, APIKeyID: owner.APIKeyID, AccountID: in.AccountID}
	} else if v.AccountID != in.AccountID {
		return ErrVideoAttemptConflict
	}
	if err := requireStudioNativeVideoReceipt(ctx, s.videoResults.repo, v); err != nil {
		return err
	}
	source, err := s.refreshStoredVideoSource(ctx, v)
	if err != nil && !errors.Is(err, errStudioNativeContent) {
		return err
	}
	return s.CompleteStudioNativeVideo(ctx, owner, resource, source, in.AccountID, true)
}

func (s *OpenAIGatewayService) refreshNativeVideoSource(ctx context.Context, account *Account, v *VideoResult) (string, error) {
	body, _, err := s.loadStudioNativeVideoStatus(ctx, account, v)
	if err != nil {
		return "", err
	}
	source := ""
	if strings.HasPrefix(v.TaskID, "dapacai:") {
		if !dapacaiCompletedStatus(body, v.TaskID) {
			return "", ErrVideoResultExpired
		}
		return "", errStudioNativeContent
	} else if strings.HasPrefix(v.TaskID, "seedance:") {
		id := gjson.GetBytes(body, "id").String()
		if gjson.GetBytes(body, "status").String() != "succeeded" || id != "" && SeedanceTaskKey(id) != v.TaskID {
			return "", ErrVideoResultExpired
		}
		source = gjson.GetBytes(body, "content.video_url").String()
	} else {
		id := extractGrokMediaVideoRequestID(body)
		if !IsGrokVideoStatusBillable(body) || id != "" && id != v.TaskID {
			return "", ErrVideoResultExpired
		}
		source, err = grokMediaSignedVideoContentURL(body, v.TaskID)
		if err == nil && source == "" {
			return "", errStudioNativeContent
		}
	}
	if err != nil || !validVideoResultURL(source) || videoSignedURLExpired(source, time.Now()) {
		return "", ErrVideoResultExpired
	}
	return source, nil
}

func (s *OpenAIGatewayService) loadStudioNativeVideoStatus(ctx context.Context, account *Account, v *VideoResult) ([]byte, string, error) {
	if s.httpUpstream == nil || account == nil || v == nil || account.ID != v.AccountID ||
		v.UserID <= 0 || v.APIKeyID <= 0 || v.Resource != "task:"+v.TaskID || !ValidVideoAttemptID(v.TaskID) {
		return nil, "", ErrVideoResultUnavailable
	}
	target, token := "", ""
	var err error
	seedance := strings.HasPrefix(v.TaskID, "seedance:")
	if strings.HasPrefix(v.TaskID, "dapacai:") && account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityDapacaiVideo) {
		target, err = dapacaiVideoURL(v.TaskID, false)
		token = strings.TrimSpace(account.GetCredential("api_key"))
	} else if seedance && account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilitySeedance) {
		base, baseErr := s.validateUpstreamBaseURL(account.GetCredential("base_url"))
		if baseErr != nil {
			return nil, "", ErrVideoResultUnavailable
		}
		target, err = buildSeedanceURL(base, SeedanceEndpointStatus, strings.TrimPrefix(v.TaskID, "seedance:"))
		token = strings.TrimSpace(account.GetCredential("api_key"))
	} else if !seedance && account.Platform == PlatformGrok {
		target, err = buildGrokMediaURL(account, s.cfg, GrokMediaEndpointVideoStatus, v.TaskID)
		if err == nil {
			token, _, err = s.getRequestCredential(ctx, nil, account)
		}
	} else {
		return nil, "", ErrVideoResultUnavailable
	}
	if err != nil || target == "" || token == "" {
		return nil, "", ErrVideoResultUnavailable
	}
	ctx, cancel := context.WithTimeout(WithHTTPUpstreamRedirectsDisabled(ctx), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", ErrVideoResultUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if account.IsGrokOAuth() && isGrokCLIProxyTarget(target) {
		applyGrokCLIHeaders(req.Header)
	}
	account.ApplyHeaderOverrides(req.Header)
	req.Header.Del("X-Studio-Library")
	req.Header.Del("X-Video-Quote")
	req.Header.Del("Idempotency-Key")
	proxy := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxy = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(req, proxy, account.ID, account.Concurrency)
	if err != nil {
		return nil, "", ErrVideoResultUnavailable
	}
	defer resp.Body.Close()
	const maxStatus = 2 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxStatus+1))
	if err != nil || len(body) > maxStatus || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", ErrVideoResultUnavailable
	}
	return body, token, nil
}

type studioVideoContentBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *studioVideoContentBody) Close() error {
	defer b.cancel()
	return b.ReadCloser.Close()
}

func (s *OpenAIGatewayService) openStudioNativeVideoContent(ctx context.Context, v *VideoResult) (*VideoResultContent, error) {
	if !studioNativeContentTask(v) || s.accountRepo == nil {
		return nil, ErrVideoResultUnavailable
	}
	account, err := s.accountRepo.GetByID(ctx, v.AccountID)
	dapacai := strings.HasPrefix(v.TaskID, "dapacai:")
	if err != nil || account == nil || (dapacai && !account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityDapacaiVideo)) ||
		(!dapacai && account.Platform != PlatformGrok) {
		return nil, ErrVideoResultUnavailable
	}
	body, token, err := s.loadStudioNativeVideoStatus(ctx, account, v)
	if err != nil {
		return nil, err
	}
	id := extractGrokMediaVideoRequestID(body)
	if dapacai && !dapacaiCompletedStatus(body, v.TaskID) || !dapacai && (!IsGrokVideoStatusBillable(body) || id != "" && id != v.TaskID) {
		return nil, ErrVideoResultExpired
	}
	target := ""
	if !dapacai {
		target, err = grokMediaSignedVideoContentURL(body, v.TaskID)
	}
	if err != nil {
		return nil, ErrVideoResultExpired
	}
	signed := target != ""
	if !signed {
		if dapacai {
			target, err = dapacaiVideoURL(v.TaskID, true)
		} else {
			target, err = buildGrokMediaURL(account, s.cfg, GrokMediaEndpointVideoContent, v.TaskID)
		}
		if err != nil {
			return nil, ErrVideoResultUnavailable
		}
	}
	ctx, cancel := context.WithTimeout(WithHTTPUpstreamRedirectsDisabled(ctx), 110*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		cancel()
		return nil, ErrVideoResultUnavailable
	}
	req.Header.Set("Accept", "*/*")
	if !signed {
		req.Header.Set("Authorization", "Bearer "+token)
		if account.IsGrokOAuth() && isGrokCLIProxyTarget(target) {
			applyGrokCLIHeaders(req.Header)
		}
		account.ApplyHeaderOverrides(req.Header)
		req.Header.Del("X-Studio-Library")
		req.Header.Del("X-Video-Quote")
		req.Header.Del("Idempotency-Key")
	}
	proxy := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxy = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(req, proxy, account.ID, account.Concurrency)
	if err != nil {
		cancel()
		return nil, ErrVideoResultUnavailable
	}
	if resp.StatusCode != http.StatusOK || resp.ContentLength > 300<<20 ||
		dapacai && !strings.HasPrefix(resp.Header.Get("Content-Type"), "video/") && resp.Header.Get("Content-Type") != "application/octet-stream" {
		resp.Body.Close()
		cancel()
		return nil, ErrVideoResultUnavailable
	}
	return &VideoResultContent{Body: &studioVideoContentBody{ReadCloser: resp.Body, cancel: cancel}, Size: resp.ContentLength}, nil
}
