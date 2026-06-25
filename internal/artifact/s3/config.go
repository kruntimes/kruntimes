package s3

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

const minimumUploadPartSize = 5 * 1024 * 1024

// Config configures an S3-compatible artifact store. Credentials are resolved
// exclusively through the AWS SDK default credential chain.
type Config struct {
	Bucket         string
	Prefix         string
	Region         string
	Endpoint       string
	ForcePathStyle bool

	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string

	UploadPartSize    int64
	UploadConcurrency int
}

func (c Config) validate() error {
	if strings.TrimSpace(c.Bucket) == "" {
		return fmt.Errorf("s3 bucket is required")
	}
	if strings.Contains(c.Bucket, "/") {
		return fmt.Errorf("s3 bucket must not contain path separators")
	}
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("invalid s3 endpoint %q", c.Endpoint)
		}
	}
	if c.UploadPartSize < 0 {
		return fmt.Errorf("s3 upload part size must not be negative")
	}
	if c.UploadPartSize > 0 && c.UploadPartSize < minimumUploadPartSize {
		return fmt.Errorf("s3 upload part size must be at least %d bytes", minimumUploadPartSize)
	}
	if c.UploadConcurrency < 0 {
		return fmt.Errorf("s3 upload concurrency must not be negative")
	}
	return nil
}

// New constructs an S3-compatible store using the AWS SDK default credential chain.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	loadOptions := make([]func(*awsconfig.LoadOptions) error, 0, 1)
	if cfg.Region != "" {
		loadOptions = append(loadOptions, awsconfig.WithRegion(cfg.Region))
	}
	if cfg.AccessKeyID != "" || cfg.SecretAccessKey != "" || cfg.SessionToken != "" {
		if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
			return nil, fmt.Errorf("s3 access key id and secret access key must both be set")
		}
		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{
				AccessKeyID:     cfg.AccessKeyID,
				SecretAccessKey: cfg.SecretAccessKey,
				SessionToken:    cfg.SessionToken,
				Source:          "kruntimes artifact store secret",
			}, nil
		})))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}

	client := awss3.NewFromConfig(awsCfg, func(options *awss3.Options) {
		options.UsePathStyle = cfg.ForcePathStyle
		if cfg.Endpoint != "" {
			options.BaseEndpoint = aws.String(cfg.Endpoint)
		}
	})
	uploader := manager.NewUploader(client, func(options *manager.Uploader) {
		if cfg.UploadPartSize > 0 {
			options.PartSize = cfg.UploadPartSize
		}
		if cfg.UploadConcurrency > 0 {
			options.Concurrency = cfg.UploadConcurrency
		}
	})

	return newStore(cfg, uploader, client), nil
}
