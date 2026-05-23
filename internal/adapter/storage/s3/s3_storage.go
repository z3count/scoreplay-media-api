// Package s3 implements the port.FileStorage interface using Amazon S3
// (or any S3-compatible service like MinIO, DigitalOcean Spaces, etc.).
//
// Files are stored with UUID-based object keys to prevent collisions and
// path traversal attacks. The original client filename is never used as
// the S3 key.
//
// This adapter is a drop-in replacement for local.Storage thanks to the
// port.FileStorage interface — no changes to the service layer required.
package s3

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"

	"github.com/scoreplay/media-api/internal/port"
)

// Storage implements port.FileStorage backed by an S3 bucket.
//
// Thread safety: this implementation is safe for concurrent use.
// Each call to Save generates a unique UUID-based key, so concurrent
// uploads never collide.
type Storage struct {
	client   *s3.Client
	uploader *transfermanager.Client
	bucket   string
	prefix   string // optional key prefix (e.g. "media/")
	cdnURL   string // optional CDN base URL for public access
}

// Config holds the S3 connection parameters.
type Config struct {
	Bucket    string // S3 bucket name (required)
	Region    string // AWS region (required, e.g. "eu-west-1")
	Endpoint  string // Custom endpoint for S3-compatible services (optional)
	AccessKey string // AWS access key ID (optional if using IAM roles)
	SecretKey string // AWS secret access key (optional if using IAM roles)
	Prefix    string // Key prefix for all objects (optional, e.g. "media/")
	CDNURL    string // CDN base URL for public URLs (optional, falls back to S3 URL)
}

// NewStorage creates a new S3 file storage client.
//
// When AccessKey and SecretKey are provided, explicit credentials are used.
// Otherwise, the SDK falls back to the default credential chain (IAM roles,
// env vars, shared config files, etc.).
//
// When Endpoint is set, the client connects to an S3-compatible service
// (e.g. MinIO) instead of AWS. PathStyle addressing is forced in that case.
func NewStorage(ctx context.Context, cfg Config) (*Storage, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("S3 bucket name is required")
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("S3 region is required")
	}

	// Build AWS config options.
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}

	// Use explicit credentials when provided.
	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	// Build S3 client options.
	s3Opts := []func(*s3.Options){}
	if cfg.Endpoint != "" {
		// Custom endpoint for S3-compatible services (MinIO, DigitalOcean Spaces, etc.).
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = true // Required for most S3-compatible services.
		})
	}

	client := s3.NewFromConfig(awsCfg, s3Opts...)

	// transfermanager.Client does multipart uploads, streaming the source
	// reader in chunks. Peak memory is roughly PartSize × Concurrency
	// (defaults: 8 MiB × 5 = ~40 MiB) regardless of total file size, so
	// even multi-GB videos never load the body into RAM. This package is
	// the AWS-recommended replacement for the now-deprecated
	// feature/s3/manager.
	uploader := transfermanager.New(client)

	return &Storage{
		client:   client,
		uploader: uploader,
		bucket:   cfg.Bucket,
		prefix:   cfg.Prefix,
		cdnURL:   cfg.CDNURL,
	}, nil
}

// Save uploads file content to S3 via multipart upload and returns the
// object key.
//
// The object key is generated as: <prefix><uuid><ext>
// Example: "media/a1b2c3d4-5678-90ab-cdef-1234567890ab.jpg"
//
// We use the transfer manager rather than the lower-level PutObject
// because PutObject is a single-shot HTTP PUT — to compute the v4
// signature it must read the body twice (requires a seekable reader) or
// buffer it entirely, which would OOM on large videos. UploadObject
// reads the source in fixed-size chunks and signs each part
// independently, so peak memory is bounded by the chunk size × upload
// concurrency regardless of the total upload size.
func (s *Storage) Save(ctx context.Context, reader io.Reader, ext string) (string, error) {
	tenantID, err := port.TenantIDFromContext(ctx)
	if err != nil {
		return "", err
	}

	// Object key shape: <prefix><tenant_id>/<uuid>.<ext>. The tenant id
	// in the key gives object-level ownership in the bucket — useful for
	// per-tenant lifecycle rules, S3 inventory reports, and ad-hoc
	// debugging without consulting the DB. The bucket policy / IAM role
	// is still the gatekeeper; the prefix is identification, not auth.
	key := s.prefix + tenantID.String() + "/" + uuid.New().String() + ext

	_, err = s.uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   reader,
	})
	if err != nil {
		return "", fmt.Errorf("upload to S3 (key=%s): %w", key, err)
	}

	return key, nil
}

// Delete removes an object from S3 by its key.
//
// S3 DeleteObject is idempotent: deleting a non-existent key does not
// return an error, which matches our port.FileStorage contract.
func (s *Storage) Delete(ctx context.Context, path string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(path),
	})
	if err != nil {
		return fmt.Errorf("delete from S3 (key=%s): %w", path, err)
	}
	return nil
}

// URL returns the public URL for a stored file.
//
// If a CDN URL is configured, it builds: <cdnURL>/<key>
// Otherwise, it builds the standard S3 URL.
//
// The baseURL parameter is ignored when a CDN URL is configured,
// maintaining compatibility with the port.FileStorage interface.
func (s *Storage) URL(baseURL, path string) string {
	if s.cdnURL != "" {
		return s.cdnURL + "/" + path
	}
	// Fallback: construct a standard S3 URL.
	return fmt.Sprintf("https://%s.s3.amazonaws.com/%s", s.bucket, path)
}

// URLWithExpiry returns a presigned GET URL for the object, valid for ttl.
//
// The URL is signed with the app's AWS credentials (whichever the SDK
// credential chain resolved at startup) and grants the bearer time-limited
// read access without any further auth. The bucket can therefore be fully
// private — the presigned URL alone authorises the read.
//
// The CDN base URL is intentionally NOT used here: CloudFront/Cloudflare
// won't recognise the S3 SigV4 query parameters and would either pass them
// through (defeating their cache) or reject the request. Private media
// flows should either skip the CDN, or use a CDN-native signing scheme
// (CloudFront signed URLs / cookies), which is a separate code path.
//
// baseURL is part of the port.FileStorage contract but unused here.
func (s *Storage) URLWithExpiry(ctx context.Context, baseURL, path string, ttl time.Duration) (string, error) {
	presigner := s3.NewPresignClient(s.client)
	req, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(path),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign GetObject (key=%s): %w", path, err)
	}
	return req.URL, nil
}
