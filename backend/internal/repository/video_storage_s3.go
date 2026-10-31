package repository

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

var _ service.VideoObjectStore = (*S3ImageStorage)(nil)

func (s *S3ImageStorage) SaveVideo(ctx context.Context, key, mime string, body io.ReadSeeker, size int64) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &s.bucket, Key: &key, Body: body, ContentType: &mime, ContentLength: &size,
	})
	return err
}

func (s *S3ImageStorage) VideoURL(ctx context.Context, key string) (string, error) {
	result, err := s3.NewPresignClient(s.client).PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket, Key: &key,
	}, s3.WithPresignExpires(15*time.Minute))
	if err != nil {
		return "", err
	}
	return result.URL, nil
}

func (s *S3ImageStorage) OpenVideo(ctx context.Context, key, byteRange string) (*service.VideoResultContent, error) {
	input := &s3.GetObjectInput{Bucket: &s.bucket, Key: &key}
	if byteRange != "" {
		input.Range = &byteRange
	}
	result, err := s.client.GetObject(ctx, input)
	if err != nil {
		return nil, err
	}
	status := http.StatusOK
	if result.ContentRange != nil {
		status = http.StatusPartialContent
	}
	return &service.VideoResultContent{
		Body: result.Body, MIME: aws.ToString(result.ContentType), Size: aws.ToInt64(result.ContentLength),
		Status: status, ContentRange: aws.ToString(result.ContentRange),
	}, nil
}
