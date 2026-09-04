package s3put

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type fakeAPI struct {
	headErr error
	puts    []*s3.PutObjectInput
	bodies  [][]byte
}

func (f *fakeAPI) HeadBucket(ctx context.Context, in *s3.HeadBucketInput, opts ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	if f.headErr != nil {
		return nil, f.headErr
	}
	return &s3.HeadBucketOutput{}, nil
}

func (f *fakeAPI) PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	b, _ := io.ReadAll(in.Body)
	f.puts = append(f.puts, in)
	f.bodies = append(f.bodies, b)
	return &s3.PutObjectOutput{}, nil
}

func TestKeyLayout(t *testing.T) {
	cases := []struct {
		prefix string
		rel    string
		want   string
	}{
		{"", "baseline.db", "baseline.db"},
		{"treadmark/", "baseline.db", "treadmark/baseline.db"},
		{"/treadmark/alma10/", "baseline.db", "treadmark/alma10/baseline.db"},
		{"a/b", "sub/x.json", "a/b/sub/x.json"},
	}
	for _, tc := range cases {
		p := NewWithClient(&fakeAPI{}, Options{Bucket: "b", Prefix: tc.prefix})
		if got := p.Key(tc.rel); got != tc.want {
			t.Errorf("prefix %q rel %q: got %q want %q", tc.prefix, tc.rel, got, tc.want)
		}
	}
	p := NewWithClient(&fakeAPI{}, Options{Bucket: "bkt", Prefix: "pre/"})
	if got := p.URL("f"); got != "s3://bkt/pre/f" {
		t.Errorf("URL: %s", got)
	}
}

func TestUploadFilePropagation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeAPI{}
	p := NewWithClient(fake, Options{
		Bucket:       "bkt",
		Prefix:       "treadmark/",
		StorageClass: "STANDARD_IA",
		SSE:          "aws:kms",
		KMSKeyID:     "key-123",
	})
	key, err := p.UploadFile(context.Background(), dir, "metadata.json", "cafe")
	if err != nil {
		t.Fatal(err)
	}
	if key != "treadmark/metadata.json" {
		t.Fatalf("key: %s", key)
	}
	in := fake.puts[0]
	if aws.ToString(in.Bucket) != "bkt" || aws.ToString(in.Key) != "treadmark/metadata.json" {
		t.Fatalf("put input: %+v", in)
	}
	if in.StorageClass != types.StorageClass("STANDARD_IA") {
		t.Fatalf("storage class: %v", in.StorageClass)
	}
	if in.ServerSideEncryption != types.ServerSideEncryption("aws:kms") || aws.ToString(in.SSEKMSKeyId) != "key-123" {
		t.Fatalf("sse: %v %v", in.ServerSideEncryption, in.SSEKMSKeyId)
	}
	if in.Metadata["sha256"] != "cafe" {
		t.Fatalf("metadata: %v", in.Metadata)
	}
	if aws.ToString(in.ContentType) != "application/json" {
		t.Fatalf("content type: %v", aws.ToString(in.ContentType))
	}
	if !bytes.Equal(fake.bodies[0], []byte(`{}`)) {
		t.Fatalf("body: %q", fake.bodies[0])
	}
}

func TestCheckError(t *testing.T) {
	p := NewWithClient(&fakeAPI{headErr: errors.New("403")}, Options{Bucket: "nope"})
	if err := p.Check(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

// TestMinIOIntegration exercises the real client against an S3-compatible
// endpoint (MinIO in CI). Gated on TREADMARK_S3_TEST_ENDPOINT.
func TestMinIOIntegration(t *testing.T) {
	endpoint := os.Getenv("TREADMARK_S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("TREADMARK_S3_TEST_ENDPOINT not set")
	}
	ctx := context.Background()
	const bucket = "treadmark-it"

	// Bucket setup is test scaffolding — the plugin itself never creates
	// buckets.
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	if _, err := raw.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Logf("create bucket (may already exist): %v", err)
	}

	p, err := New(ctx, Options{
		Bucket:         bucket,
		Prefix:         "treadmark/it/",
		Endpoint:       endpoint,
		ForcePathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Check(ctx); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	content := []byte("baseline-bytes")
	if err := os.WriteFile(filepath.Join(dir, "baseline.db"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	key, err := p.UploadFile(ctx, dir, "baseline.db", "beef")
	if err != nil {
		t.Fatal(err)
	}

	got, err := raw.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	back, _ := io.ReadAll(got.Body)
	if !bytes.Equal(back, content) {
		t.Fatalf("round-trip mismatch: %q", back)
	}
	if got.Metadata["sha256"] != "beef" {
		t.Fatalf("metadata lost: %v", got.Metadata)
	}
}
