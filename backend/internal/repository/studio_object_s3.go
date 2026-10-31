package repository

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

var _ service.StudioAssetObjectStore = (*S3ImageStorage)(nil)

func (s *S3ImageStorage) OpenStudioAsset(ctx context.Context, key, byteRange string) (*service.VideoResultContent, error) {
	return s.OpenVideo(ctx, key, byteRange)
}

func (s *S3ImageStorage) StudioAssetURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	if expiry <= 0 || expiry > 15*time.Minute {
		expiry = 15 * time.Minute
	}
	result, err := s3.NewPresignClient(s.client).PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket, Key: &key,
	}, s3.WithPresignExpires(expiry))
	if err != nil {
		return "", err
	}
	return result.URL, nil
}
