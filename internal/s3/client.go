package s3

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type S3Client struct {
	client         *s3.Client
	bucket         string
	defaultTimeout time.Duration
}

func NewS3Client(accessKey string, secretKey string, endpoint string, region string, bucket string, defaultTimeout time.Duration) (*S3Client, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, err
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	return &S3Client{
		client:         client,
		bucket:         bucket,
		defaultTimeout: defaultTimeout,
	}, nil
}

func (c *S3Client) GetFile(ctx context.Context, key string) ([]byte, error) {
	fmt.Println("defaultTimeout", c.defaultTimeout)

	getFileCtx, cancel := context.WithTimeout(ctx, c.defaultTimeout)
	defer cancel()

	result, err := c.client.GetObject(getFileCtx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get object from S3: %w", err)
	}

	bodyBytes, err := io.ReadAll(result.Body) // Read the body to ensure it is not nil
	if err != nil {
		return nil, fmt.Errorf("failed to read object body: %w", err)
	}
	if result.Body == nil {
		return nil, fmt.Errorf("object body is nil for key: %s", key)
	}

	return bodyBytes, nil
}
