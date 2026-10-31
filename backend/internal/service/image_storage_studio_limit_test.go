package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type studioLimitSettings struct {
	SettingRepository
	fail bool
}

func (r *studioLimitSettings) GetValue(context.Context, string) (string, error) {
	if r.fail {
		return "", errors.New("settings unavailable")
	}
	return "", nil
}

// Losing the limit on a disabled/incomplete/failed binding would allow oversized pending SourceBytes into PostgreSQL.
func TestStudioAssetUnavailableSettingsPreserveEffectiveLimit(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		enabled, incomplete, factoryFail bool
	}{
		{"disabled", false, false, false}, {"incomplete", true, true, false}, {"factory unavailable", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.ImageStorageConfig{Enabled: tc.enabled, Bucket: "private", AccessKeyID: "synthetic", SecretAccessKey: "synthetic", MaxDownloadByte: 32}
			if tc.incomplete {
				cfg.AccessKeyID = ""
			}
			factoryCalls := 0
			settings := NewImageStorageSettingService(&studioLimitSettings{}, nil, nil, func(context.Context, *config.ImageStorageConfig) (ImageStorage, error) {
				factoryCalls++
				return nil, errors.New("storage unavailable")
			}, cfg)
			u, enabled := settings.Resolver()()
			require.False(t, enabled)
			require.NotNil(t, u)
			require.Equal(t, int64(32), u.maxDownloadBytes)
			_, err := u.Save(context.Background(), "must-not-save", "image/png", studioPNG(t))
			require.Error(t, err)
			r := &studioArchiveMemory{}
			s := NewStudioAssetService(r, settings.Resolver())
			require.ErrorIs(t, s.CollectImage(context.Background(), studioArchiveOwner, "generation", 0, "model", "prompt", nil, StudioImageOutput{Bytes: studioPNG(t), MIME: "image/png"}), ErrStudioAssetInvalidRequest)
			require.Nil(t, r.asset)
			if tc.factoryFail {
				require.Equal(t, 1, factoryCalls)
			} else {
				require.Zero(t, factoryCalls)
			}
		})
	}
}

func TestStudioAssetSettingsLoadFailureCannotGuessEffectiveLimit(t *testing.T) {
	settingsRepo := &studioLimitSettings{fail: true}
	settings := NewImageStorageSettingService(settingsRepo, nil, nil, nil, config.ImageStorageConfig{MaxDownloadByte: 1024})
	r := &studioArchiveMemory{}
	s := NewStudioAssetService(r, settings.Resolver())
	require.ErrorIs(t, s.CollectImage(context.Background(), studioArchiveOwner, "generation", 0, "model", "prompt", nil, StudioImageOutput{Bytes: studioPNG(t), MIME: "image/png"}), ErrStudioAssetUnavailable)
	require.Nil(t, r.asset)
	settingsRepo.fail = false
	require.ErrorIs(t, s.CollectImage(context.Background(), studioArchiveOwner, "generation", 0, "model", "prompt", nil, StudioImageOutput{Bytes: studioPNG(t), MIME: "image/png"}), ErrStudioAssetUnavailable)
	require.NotNil(t, r.asset, "a transient settings outage must not permanently cache an unknown source-size limit")
	require.NotEmpty(t, r.asset.SourceBytes)
}
