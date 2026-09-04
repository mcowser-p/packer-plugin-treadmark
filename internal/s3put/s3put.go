// Package s3put uploads baseline bundles to S3 or any S3-compatible store.
// It uses the default AWS credential chain, requires the bucket to already
// exist (HeadBucket check — this plugin never creates buckets), and supports
// endpoint override + path-style addressing for MinIO and friends.
package s3put

import (
	"context"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Options configures a Putter. Zero values mean "use the default chain /
// AWS defaults".
type Options struct {
	Bucket         string
	Prefix         string
	Region         string
	Endpoint       string
	ForcePathStyle bool
	StorageClass   string // e.g. STANDARD, STANDARD_IA
	SSE            string // "", "AES256", "aws:kms"
	KMSKeyID       string
}

// API is the S3 surface this package needs; satisfied by *s3.Client and by
// test fakes.
type API interface {
	HeadBucket(ctx context.Context, in *s3.HeadBucketInput, opts ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

type Putter struct {
	c API
	o Options
}

// New builds a Putter backed by a real S3 client using the default AWS
// credential chain.
func New(ctx context.Context, o Options) (*Putter, error) {
	var loadOpts []func(*awsconfig.LoadOptions) error
	if o.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(o.Region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	client := s3.NewFromConfig(cfg, func(so *s3.Options) {
		if o.Endpoint != "" {
			so.BaseEndpoint = aws.String(o.Endpoint)
		}
		so.UsePathStyle = o.ForcePathStyle
	})
	return &Putter{c: client, o: o}, nil
}

// NewWithClient builds a Putter over an explicit API implementation (tests).
func NewWithClient(c API, o Options) *Putter {
	return &Putter{c: c, o: o}
}

// Check verifies the target bucket exists and is reachable. The plugin never
// creates buckets.
func (p *Putter) Check(ctx context.Context) error {
	_, err := p.c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(p.o.Bucket)})
	if err != nil {
		return fmt.Errorf("bucket %q not reachable (this plugin never creates buckets — create it first): %w", p.o.Bucket, err)
	}
	return nil
}

// Key returns the object key for a bundle-relative file name.
func (p *Putter) Key(rel string) string {
	prefix := strings.Trim(p.o.Prefix, "/")
	rel = strings.TrimLeft(filepath.ToSlash(rel), "/")
	if prefix == "" {
		return rel
	}
	return prefix + "/" + rel
}

// URL returns the s3:// URL for a bundle-relative file name.
func (p *Putter) URL(rel string) string {
	return "s3://" + p.o.Bucket + "/" + p.Key(rel)
}

func contentType(name string) string {
	if ct := mime.TypeByExtension(filepath.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// UploadFile uploads dir/rel to Key(rel) and returns the object key.
func (p *Putter) UploadFile(ctx context.Context, dir, rel, sha256hex string) (string, error) {
	f, err := os.Open(filepath.Join(dir, rel))
	if err != nil {
		return "", err
	}
	defer f.Close()
	in := &s3.PutObjectInput{
		Bucket:      aws.String(p.o.Bucket),
		Key:         aws.String(p.Key(rel)),
		Body:        f,
		ContentType: aws.String(contentType(rel)),
	}
	if p.o.StorageClass != "" {
		in.StorageClass = types.StorageClass(p.o.StorageClass)
	}
	if p.o.SSE != "" {
		in.ServerSideEncryption = types.ServerSideEncryption(p.o.SSE)
		if p.o.SSE == "aws:kms" && p.o.KMSKeyID != "" {
			in.SSEKMSKeyId = aws.String(p.o.KMSKeyID)
		}
	}
	if sha256hex != "" {
		in.Metadata = map[string]string{"sha256": sha256hex}
	}
	if _, err := p.c.PutObject(ctx, in); err != nil {
		return "", fmt.Errorf("uploading %s to %s: %w", rel, p.URL(rel), err)
	}
	return p.Key(rel), nil
}
