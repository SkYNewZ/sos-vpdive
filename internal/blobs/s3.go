package blobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/SkYNewZ/sos-vpdive/internal/telemetry"
)

const (
	tracerName = "github.com/SkYNewZ/sos-vpdive/internal/blobs"
	// A bucket that does not answer must not freeze a member's request.
	callTimeout = 30 * time.Second
	maxRetries  = 3
)

// S3 stores objects in a private S3-compatible bucket. minio-go adds no
// trace header to its requests: trace context never leaves (spec §9.9).
type S3 struct {
	client *minio.Client
	bucket string
	tracer trace.Tracer
}

// newS3 returns a client for cfg. Path-style addressing suits R2 and any
// S3-compatible endpoint; the fixed region avoids a bucket location lookup.
// The transport lets tests trust their TLS server; nil means minio-go's default.
func newS3(cfg config.S3, transport http.RoundTripper) (*S3, error) {
	client, err := minio.New(cfg.Endpoint.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure:       cfg.Endpoint.Scheme == "https",
		Transport:    transport,
		Region:       cfg.Region,
		BucketLookup: minio.BucketLookupPath,
		MaxRetries:   maxRetries,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 client: %w", err)
	}
	return &S3{client: client, bucket: cfg.Bucket, tracer: otel.Tracer(tracerName)}, nil
}

// Put uploads an object.
func (s *S3) Put(ctx context.Context, key string, data []byte) error {
	return s.call(ctx, "blobs.put", func(ctx context.Context) error {
		_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)),
			minio.PutObjectOptions{ContentType: "application/octet-stream"})
		return err
	})
}

// Get downloads an object.
func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	var data []byte
	err := s.call(ctx, "blobs.get", func(ctx context.Context) (err error) {
		obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
		if err != nil {
			return getError(err)
		}
		defer func() { err = errors.Join(err, obj.Close()) }()
		if data, err = io.ReadAll(obj); err != nil {
			return getError(err)
		}
		return nil
	})
	return data, err
}

func getError(err error) error {
	if minio.ToErrorResponse(err).Code == "NoSuchKey" {
		return ErrNotFound
	}
	return err
}

// Delete removes an object; S3 answers a missing key with success.
func (s *S3) Delete(ctx context.Context, key string) error {
	return s.call(ctx, "blobs.delete", func(ctx context.Context) error {
		return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
	})
}

// List returns every object of the bucket. The channel is always drained:
// minio-go's producer goroutine would leak otherwise.
func (s *S3) List(ctx context.Context) ([]Object, error) {
	var out []Object
	err := s.call(ctx, "blobs.list", func(ctx context.Context) error {
		var listErr error
		for info := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Recursive: true}) {
			if info.Err != nil {
				if listErr == nil {
					listErr = info.Err
				}
				continue
			}
			out = append(out, Object{Key: info.Key, Modified: info.LastModified.UTC()})
		}
		return listErr
	})
	return out, err
}

// call bounds fn by callTimeout inside a span named after the operation. It is
// the one place that scrubs transport errors of the object key.
func (s *S3) call(ctx context.Context, name string, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := telemetry.Trace(ctx, s.tracer, name, fn); err != nil {
		return wrap(name, err)
	}
	return nil
}
