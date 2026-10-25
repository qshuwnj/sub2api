package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type VideoReferenceKind string

const (
	VideoReferenceImage VideoReferenceKind = "image"
	VideoReferenceVideo VideoReferenceKind = "video"
	VideoReferenceAudio VideoReferenceKind = "audio"
)

type VideoReferenceCleanupState string

const (
	VideoReferenceStaged    VideoReferenceCleanupState = "staged"
	VideoReferenceAccepted  VideoReferenceCleanupState = "accepted"
	VideoReferenceCancelled VideoReferenceCleanupState = "cancelled"
	VideoReferenceExpired   VideoReferenceCleanupState = "expired"
	VideoReferenceFailed    VideoReferenceCleanupState = "failed"
)

type VideoReferenceAsset struct {
	ID              string                     `json:"id"`
	UserID          int64                      `json:"user_id"`
	APIKeyID        int64                      `json:"api_key_id"`
	AccountID       int64                      `json:"account_id,omitempty"`
	EndpointFamily  string                     `json:"endpoint_family"`
	Kind            VideoReferenceKind         `json:"kind"`
	MIME            string                     `json:"mime"`
	Size            int64                      `json:"size"`
	Width           int                        `json:"width,omitempty"`
	Height          int                        `json:"height,omitempty"`
	Duration        float64                    `json:"duration,omitempty"`
	FrameRate       float64                    `json:"frame_rate,omitempty"`
	StorageKey      string                     `json:"storage_key"`
	SourceURL       string                     `json:"source_url,omitempty"`
	ExpiresAt       time.Time                  `json:"expires_at"`
	State           VideoReferenceCleanupState `json:"cleanup_state"`
	AcceptedAttempt string                     `json:"accepted_attempt_id,omitempty"`
}

type VideoReferenceMediaMetadata struct {
	Kind      VideoReferenceKind
	MIME      string
	Width     int
	Height    int
	Duration  float64
	FrameRate float64
}

type ffprobeDocument struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		Duration  string `json:"duration"`
		FrameRate string `json:"r_frame_rate"`
		AvgRate   string `json:"avg_frame_rate"`
	} `json:"streams"`
	Format struct {
		Duration   string `json:"duration"`
		FormatName string `json:"format_name"`
	} `json:"format"`
}

type VideoReferenceFFProbeRunner func(context.Context, ...string) ([]byte, error)

type VideoReferenceFFProbe struct {
	Runner         VideoReferenceFFProbeRunner
	ReaderRunner   func(context.Context, ...string) (io.ReadCloser, error)
	Timeout        time.Duration
	MaxOutputBytes int
}

func NewVideoReferenceFFProbe(runner VideoReferenceFFProbeRunner) *VideoReferenceFFProbe {
	readerRunner := func(ctx context.Context, args ...string) (io.ReadCloser, error) {
		cmd := exec.CommandContext(ctx, "ffprobe", args...)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return &ffprobeReadCloser{ReadCloser: stdout, wait: cmd.Wait}, nil
	}
	return &VideoReferenceFFProbe{Runner: runner, ReaderRunner: readerRunner, Timeout: 10 * time.Second, MaxOutputBytes: 1 << 20}
}

type ffprobeReadCloser struct {
	io.ReadCloser
	wait func() error
}

func (r *ffprobeReadCloser) Close() error {
	closeErr := r.ReadCloser.Close()
	waitErr := r.wait()
	if closeErr != nil {
		return closeErr
	}
	return waitErr
}

func parseFFProbeFloat(value string) (float64, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "N/A" {
		return 0, nil
	}
	if strings.Contains(value, "/") {
		parts := strings.SplitN(value, "/", 2)
		numerator, err1 := strconv.ParseFloat(parts[0], 64)
		denominator, err2 := strconv.ParseFloat(parts[1], 64)
		if err1 != nil || err2 != nil || denominator == 0 {
			return 0, fmt.Errorf("invalid ffprobe fraction")
		}
		return numerator / denominator, nil
	}
	return strconv.ParseFloat(value, 64)
}

func (p *VideoReferenceFFProbe) Probe(ctx context.Context, path string) (VideoReferenceMediaMetadata, error) {
	if p == nil || (p.Runner == nil && p.ReaderRunner == nil) || strings.TrimSpace(path) == "" {
		return VideoReferenceMediaMetadata{}, errors.New("ffprobe is not configured")
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	maxOutput := p.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = 1 << 20
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := []string{"-v", "error", "-print_format", "json", "-show_streams", "-show_format", path}
	var raw []byte
	var err error
	if p.Runner == nil && p.ReaderRunner != nil {
		reader, openErr := p.ReaderRunner(probeCtx, args...)
		if openErr != nil {
			return VideoReferenceMediaMetadata{}, fmt.Errorf("ffprobe: %w", openErr)
		}
		raw, err = io.ReadAll(io.LimitReader(reader, int64(maxOutput)+1))
		closeErr := reader.Close()
		if len(raw) > maxOutput {
			return VideoReferenceMediaMetadata{}, errors.New("ffprobe output exceeds bounded limit")
		}
		if err == nil && closeErr != nil {
			err = closeErr
		}
	} else {
		raw, err = p.Runner(probeCtx, args...)
	}
	if err != nil {
		return VideoReferenceMediaMetadata{}, fmt.Errorf("ffprobe: %w", err)
	}
	if len(raw) > maxOutput {
		return VideoReferenceMediaMetadata{}, errors.New("ffprobe output exceeds bounded limit")
	}
	var doc ffprobeDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return VideoReferenceMediaMetadata{}, fmt.Errorf("ffprobe JSON: %w", err)
	}
	var out VideoReferenceMediaMetadata
	for _, stream := range doc.Streams {
		switch stream.CodecType {
		case "video":
			out.Kind = VideoReferenceVideo
			out.Width, out.Height = stream.Width, stream.Height
			if stream.Duration != "" {
				out.Duration, _ = parseFFProbeFloat(stream.Duration)
			}
			if out.Duration == 0 {
				out.Duration, _ = parseFFProbeFloat(doc.Format.Duration)
			}
			if stream.FrameRate != "" {
				out.FrameRate, _ = parseFFProbeFloat(stream.FrameRate)
			}
			if out.FrameRate == 0 && stream.AvgRate != "" {
				out.FrameRate, _ = parseFFProbeFloat(stream.AvgRate)
			}
		case "audio":
			if out.Kind == "" {
				out.Kind = VideoReferenceAudio
			}
			if out.Duration == 0 && stream.Duration != "" {
				out.Duration, _ = parseFFProbeFloat(stream.Duration)
			}
		}
	}
	if out.Duration == 0 {
		out.Duration, _ = parseFFProbeFloat(doc.Format.Duration)
	}
	if out.Kind == "" {
		return VideoReferenceMediaMetadata{}, errors.New("ffprobe returned no audio/video stream")
	}
	format := strings.ToLower(doc.Format.FormatName)
	switch {
	case strings.Contains(format, "webm") || strings.Contains(format, "matroska"):
		if out.Kind == VideoReferenceVideo {
			out.MIME = "video/webm"
		} else {
			out.MIME = "audio/webm"
		}
	case strings.Contains(format, "mov") || strings.Contains(format, "mp4") || strings.Contains(format, "m4a") || strings.Contains(format, "3gp"):
		if out.Kind == VideoReferenceVideo {
			out.MIME = "video/mp4"
		} else {
			out.MIME = "audio/mp4"
		}
	case strings.Contains(format, "wav"):
		out.MIME = "audio/wav"
	case strings.Contains(format, "ogg"):
		out.MIME = "audio/ogg"
	case strings.Contains(format, "mp3") || strings.Contains(format, "mp2"):
		out.MIME = "audio/mpeg"
	default:
		return VideoReferenceMediaMetadata{}, errors.New("ffprobe returned an unsupported media container")
	}
	return out, nil
}

type videoReferenceLimits struct {
	MaxBytes                 int64
	MinDuration, MaxDuration float64
}

func videoReferenceLimit(family string, kind VideoReferenceKind) (videoReferenceLimits, bool) {
	if kind == VideoReferenceImage {
		return videoReferenceLimits{MaxBytes: 30 << 20}, family == "2.0" || family == "2.5" || family == "2.0-mini" || family == "2.0-fast"
	}
	maxDuration := 15.0
	maxVideoBytes := int64(50 << 20)
	if family == "2.5" {
		maxDuration, maxVideoBytes = 30, 200<<20
	}
	if family != "2.0" && family != "2.5" && family != "2.0-mini" && family != "2.0-fast" {
		return videoReferenceLimits{}, false
	}
	return videoReferenceLimits{MaxBytes: map[VideoReferenceKind]int64{VideoReferenceVideo: maxVideoBytes, VideoReferenceAudio: 15 << 20}[kind], MinDuration: 2, MaxDuration: maxDuration}, true
}

func ValidateVideoReferenceDescriptor(asset *VideoReferenceAsset) error {
	if asset == nil || asset.UserID <= 0 || asset.APIKeyID <= 0 || asset.ID == "" {
		return errors.New("invalid reference asset owner or ID")
	}
	limits, ok := videoReferenceLimit(asset.EndpointFamily, asset.Kind)
	if !ok {
		return errors.New("unsupported reference endpoint family")
	}
	if asset.Size <= 0 || asset.Size > limits.MaxBytes {
		return fmt.Errorf("reference %s exceeds byte limit", asset.Kind)
	}
	if asset.Kind == VideoReferenceImage {
		aspect := float64(asset.Width) / float64(asset.Height)
		if asset.Width < 300 || asset.Height < 300 || asset.Width > 6000 || asset.Height > 6000 || aspect < 0.4 || aspect > 2.5 {
			return errors.New("invalid image dimensions")
		}
		if asset.MIME != "image/png" && asset.MIME != "image/jpeg" && asset.MIME != "image/webp" {
			return errors.New("unsupported image MIME")
		}
		return nil
	}
	if math.IsNaN(asset.Duration) || math.IsInf(asset.Duration, 0) || asset.Duration < limits.MinDuration || asset.Duration > limits.MaxDuration {
		return errors.New("reference duration outside family bounds")
	}
	if asset.Kind == VideoReferenceVideo {
		pixels := asset.Width * asset.Height
		maxPixels := 2086876
		if asset.EndpointFamily == "2.5" {
			maxPixels = 8295044
		}
		aspect := float64(asset.Width) / float64(asset.Height)
		if asset.Width < 300 || asset.Height < 300 || asset.Width > 6000 || asset.Height > 6000 || pixels < 409600 || pixels > maxPixels || aspect < 0.4 || aspect > 2.5 {
			return errors.New("invalid video dimensions")
		}
		if asset.FrameRate < 24 || asset.FrameRate > 60 || math.IsNaN(asset.FrameRate) {
			return errors.New("invalid video frame rate")
		}
		if asset.MIME != "video/mp4" && asset.MIME != "video/webm" && asset.MIME != "video/quicktime" {
			return errors.New("unsupported video MIME")
		}
	} else if asset.Kind == VideoReferenceAudio {
		if asset.MIME != "audio/mpeg" && asset.MIME != "audio/wav" && asset.MIME != "audio/ogg" && asset.MIME != "audio/mp4" && asset.MIME != "audio/webm" {
			return errors.New("unsupported audio MIME")
		}
	} else {
		return errors.New("unsupported reference media kind")
	}
	return nil
}

// ValidateVideoReferenceSet applies the native family count, per-item and
// combined A/V admission rules before a quote or attempt can reference assets.
func ValidateVideoReferenceSet(assets []*VideoReferenceAsset, family string) error {
	if len(assets) == 0 {
		return errors.New("at least one reference asset is required")
	}
	maxImages, maxVideos, maxAudio, maxDuration := 9, 3, 3, 15.0
	if family == "2.5" {
		maxImages, maxVideos, maxAudio, maxDuration = 30, 10, 10, 30
	}
	if family != "2.0" && family != "2.5" && family != "2.0-mini" && family != "2.0-fast" {
		return errors.New("unsupported reference endpoint family")
	}
	images, videos, audio := 0, 0, 0
	total := 0.0
	for _, asset := range assets {
		if asset == nil || asset.EndpointFamily != family {
			return errors.New("reference asset family mismatch")
		}
		if err := ValidateVideoReferenceDescriptor(asset); err != nil {
			return err
		}
		switch asset.Kind {
		case VideoReferenceImage:
			images++
		case VideoReferenceVideo:
			videos++
			total += asset.Duration
		case VideoReferenceAudio:
			audio++
			total += asset.Duration
		default:
			return errors.New("unsupported reference media kind")
		}
	}
	if images > maxImages || videos > maxVideos || audio > maxAudio {
		return errors.New("reference asset count exceeds family limit")
	}
	if total > maxDuration {
		return errors.New("combined audio/video duration exceeds family budget")
	}
	if family != "2.5" && audio > 0 && videos == 0 {
		return errors.New("audio-only references are unavailable for this family")
	}
	return nil
}

type VideoReferenceAssetStore struct {
	mu             sync.Mutex
	now            func() time.Time
	items          map[string]*VideoReferenceAsset
	failedAttempts map[string]bool
}

// VideoReferenceAssetService is the gateway-facing boundary used by handlers
// and quote/attempt admission. The store is deliberately injectable so a
// durable repository can be supplied without changing validation semantics.
type VideoReferenceAssetService struct {
	store *VideoReferenceAssetStore
	probe *VideoReferenceFFProbe
}

func NewVideoReferenceAssetService(store *VideoReferenceAssetStore, probe *VideoReferenceFFProbe) *VideoReferenceAssetService {
	if store == nil {
		store = NewVideoReferenceAssetStore(nil)
	}
	if probe == nil {
		probe = NewVideoReferenceFFProbe(nil)
	}
	return &VideoReferenceAssetService{store: store, probe: probe}
}

func (s *VideoReferenceAssetService) Store() *VideoReferenceAssetStore {
	if s == nil {
		return nil
	}
	return s.store
}
func (s *VideoReferenceAssetService) FFProbe() *VideoReferenceFFProbe {
	if s == nil {
		return nil
	}
	return s.probe
}
func (s *VideoReferenceAssetService) Admit(asset *VideoReferenceAsset) error {
	if s == nil || s.store == nil {
		return errors.New("reference asset service unavailable")
	}
	return s.store.Put(asset)
}
func (s *VideoReferenceAssetService) ResolveOwned(id string, userID, apiKeyID int64, accountID ...int64) (*VideoReferenceAsset, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("reference asset service unavailable")
	}
	return s.store.GetOwned(id, userID, apiKeyID, accountID...)
}
func (s *VideoReferenceAssetService) Accept(id, attemptID string) error {
	if s == nil || s.store == nil {
		return errors.New("reference asset service unavailable")
	}
	return s.store.MarkAccepted(id, attemptID)
}
func (s *VideoReferenceAssetService) Cleanup() int {
	if s == nil || s.store == nil {
		return 0
	}
	return s.store.Cleanup()
}

func NewVideoReferenceAssetStore(now func() time.Time) *VideoReferenceAssetStore {
	if now == nil {
		now = time.Now
	}
	return &VideoReferenceAssetStore{now: now, items: map[string]*VideoReferenceAsset{}, failedAttempts: map[string]bool{}}
}

func cloneVideoReferenceAsset(asset *VideoReferenceAsset) *VideoReferenceAsset {
	if asset == nil {
		return nil
	}
	copy := *asset
	return &copy
}

func (s *VideoReferenceAssetStore) Put(asset *VideoReferenceAsset) error {
	if s == nil {
		return errors.New("asset store unavailable")
	}
	if err := ValidateVideoReferenceDescriptor(asset); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[asset.ID] = cloneVideoReferenceAsset(asset)
	return nil
}

func (s *VideoReferenceAssetStore) GetOwned(id string, userID, apiKeyID int64, accountID ...int64) (*VideoReferenceAsset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	asset := s.items[id]
	if asset == nil {
		return nil, errors.New("reference asset not found")
	}
	boundAccount := int64(0)
	if len(accountID) > 0 {
		boundAccount = accountID[0]
	}
	if asset.UserID != userID || asset.APIKeyID != apiKeyID || (asset.AccountID != 0 && asset.AccountID != boundAccount) {
		return nil, errors.New("reference asset owner mismatch")
	}
	if asset.ExpiresAt.Before(s.now()) && asset.AcceptedAttempt == "" {
		asset.State = VideoReferenceExpired
		delete(s.items, id)
		return nil, errors.New("reference asset expired")
	}
	return cloneVideoReferenceAsset(asset), nil
}

func (s *VideoReferenceAssetStore) MarkAccepted(id, attemptID string) error {
	if strings.TrimSpace(attemptID) == "" {
		return errors.New("accepted attempt ID is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	asset := s.items[id]
	if asset == nil {
		return errors.New("reference asset not found")
	}
	asset.AcceptedAttempt, asset.State = attemptID, VideoReferenceAccepted
	return nil
}
func (s *VideoReferenceAssetStore) MarkAttemptFailed(attemptID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failedAttempts[attemptID] = true
}
func (s *VideoReferenceAssetStore) Cleanup() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for id, asset := range s.items {
		if (asset.AcceptedAttempt != "" && s.failedAttempts[asset.AcceptedAttempt]) || asset.State == VideoReferenceCancelled || (asset.ExpiresAt.Before(s.now()) && asset.AcceptedAttempt == "") {
			delete(s.items, id)
			removed++
		}
	}
	return removed
}

func privateVideoReferenceIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || !ip.IsGlobalUnicast()
}

func ValidateVideoReferenceURL(raw string, lookup func(string) ([]string, error)) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return errors.New("reference URL must use HTTPS")
	}
	if u.User != nil {
		return errors.New("reference URL must not contain credentials")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		if privateVideoReferenceIP(ip) {
			return errors.New("reference URL targets a private network")
		}
		return nil
	}
	if lookup == nil {
		lookup = func(host string) ([]string, error) {
			records, err := net.LookupIP(host)
			if err != nil {
				return nil, err
			}
			out := make([]string, 0, len(records))
			for _, record := range records {
				out = append(out, record.String())
			}
			return out, nil
		}
	}
	addresses, err := lookup(u.Hostname())
	if err != nil || len(addresses) == 0 {
		return errors.New("reference URL DNS lookup failed")
	}
	for _, value := range addresses {
		if ip := net.ParseIP(value); ip == nil || privateVideoReferenceIP(ip) {
			return errors.New("reference URL resolves to a private network")
		}
	}
	return nil
}
