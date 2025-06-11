package s3

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type S3Client struct {
	client *s3.Client
	bucket string
}

func NewS3Client(region, endpoint, bucket string, isLocal bool) (*S3Client, error) {
	var cfg aws.Config
	var err error

	if isLocal {
		cfg, err = config.LoadDefaultConfig(context.TODO(),
			config.WithRegion(region),
			config.WithEndpointResolverWithOptions(aws.EndpointResolverWithOptionsFunc(
				func(service, region string, options ...interface{}) (aws.Endpoint, error) {
					return aws.Endpoint{
						PartitionID:   "aws",
						URL:           endpoint,
						SigningRegion: region,
					}, nil
				})),
		)
	} else {
		cfg, err = config.LoadDefaultConfig(context.TODO(),
			config.WithRegion(region),
		)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	var client *s3.Client
	if isLocal {
		client = s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.UsePathStyle = true
		})
	} else {
		client = s3.NewFromConfig(cfg)
	}

	s3Client := &S3Client{
		client: client,
		bucket: bucket,
	}

	return s3Client, nil
}

func (c *S3Client) GetFile(ctx context.Context, key string) (io.ReadCloser, error) {
	result, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get object from S3: %w", err)
	}

	return result.Body, nil
}
