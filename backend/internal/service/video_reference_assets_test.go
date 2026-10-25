package service

import (
	"context"
	"testing"
	"time"
)

func TestVideoReferenceFFProbeParsesBoundedMetadata(t *testing.T) {
	probe := NewVideoReferenceFFProbe(func(context.Context, ...string) ([]byte, error) {
		return []byte(`{"streams":[{"codec_type":"video","codec_name":"h264","width":1920,"height":1080,"duration":"8.25","r_frame_rate":"30000/1001"}],"format":{"duration":"8.25"}}`), nil
	})
	got, err := probe.Probe(context.Background(), "/tmp/reference.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != VideoReferenceVideo || got.MIME != "video/mp4" || got.Width != 1920 || got.Height != 1080 || got.Duration != 8.25 || got.FrameRate < 29.9 || got.FrameRate > 30.0 {
		t.Fatalf("unexpected metadata: %+v", got)
	}
}

func TestVideoReferenceFFProbeRejectsOversizedOutput(t *testing.T) {
	probe := NewVideoReferenceFFProbe(func(context.Context, ...string) ([]byte, error) {
		return make([]byte, 2<<20), nil
	})
	probe.MaxOutputBytes = 1024
	if _, err := probe.Probe(context.Background(), "reference.mp4"); err == nil {
		t.Fatal("expected bounded ffprobe output rejection")
	}
}

func TestVideoReferenceAssetStoreEnforcesOwnershipExpiryAndAcceptedReplay(t *testing.T) {
	now := time.Unix(1000, 0)
	store := NewVideoReferenceAssetStore(func() time.Time { return now })
	asset := &VideoReferenceAsset{ID: "asset-1", UserID: 7, APIKeyID: 8, EndpointFamily: "2.5", Kind: VideoReferenceImage, MIME: "image/png", Size: 4, Width: 1, Height: 1, ExpiresAt: now.Add(time.Minute), State: VideoReferenceStaged}
	if err := store.Put(asset); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetOwned("asset-1", 7, 9); err == nil {
		t.Fatal("cross-owner lookup unexpectedly succeeded")
	}
	if _, err := store.GetOwned("asset-1", 7, 8); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAccepted("asset-1", "attempt-1"); err != nil {
		t.Fatal(err)
	}
	if removed := store.Cleanup(); removed != 0 {
		t.Fatalf("accepted pending asset removed: %d", removed)
	}
	store.MarkAttemptFailed("attempt-1")
	if removed := store.Cleanup(); removed != 1 {
		t.Fatalf("failed attempt asset count = %d", removed)
	}
	if err := store.Put(&VideoReferenceAsset{ID: "expired", UserID: 7, APIKeyID: 8, EndpointFamily: "2.0", Kind: VideoReferenceImage, MIME: "image/png", Size: 4, Width: 1, Height: 1, ExpiresAt: now.Add(-time.Second), State: VideoReferenceStaged}); err != nil {
		t.Fatal(err)
	}
	if removed := store.Cleanup(); removed != 1 {
		t.Fatalf("expired asset count = %d", removed)
	}
}

func TestVideoReferenceURLAdmissionRejectsCredentialsAndPrivateNetworks(t *testing.T) {
	if err := ValidateVideoReferenceURL("http://example.com/a.mp4", nil); err == nil {
		t.Fatal("expected HTTPS rejection")
	}
	if err := ValidateVideoReferenceURL("https://user:pass@example.com/a.mp4", nil); err == nil {
		t.Fatal("expected credential rejection")
	}
	lookup := func(string) ([]string, error) { return []string{"127.0.0.1"}, nil }
	if err := ValidateVideoReferenceURL("https://cdn.example/a.mp4", lookup); err == nil {
		t.Fatal("expected DNS private-network rejection")
	}
}

func TestVideoReferenceSetAppliesJointDurationAndAudioOnlyRules(t *testing.T) {
	base := func(kind VideoReferenceKind, duration float64) *VideoReferenceAsset {
		return &VideoReferenceAsset{ID: string(kind), UserID: 1, APIKeyID: 1, EndpointFamily: "2.0", Kind: kind, MIME: map[VideoReferenceKind]string{VideoReferenceImage: "image/png", VideoReferenceVideo: "video/mp4", VideoReferenceAudio: "audio/mpeg"}[kind], Size: 4, Width: 32, Height: 32, Duration: duration, FrameRate: 30, ExpiresAt: time.Now().Add(time.Hour)}
	}
	if err := ValidateVideoReferenceSet([]*VideoReferenceAsset{base(VideoReferenceAudio, 4)}, "2.0"); err == nil {
		t.Fatal("expected 2.0 audio-only rejection")
	}
	if err := ValidateVideoReferenceSet([]*VideoReferenceAsset{base(VideoReferenceVideo, 10), base(VideoReferenceAudio, 6)}, "2.0"); err == nil {
		t.Fatal("expected joint A/V duration rejection")
	}
	base25 := base(VideoReferenceAudio, 4)
	base25.EndpointFamily = "2.5"
	if err := ValidateVideoReferenceSet([]*VideoReferenceAsset{base25}, "2.5"); err != nil {
		t.Fatal(err)
	}
}
