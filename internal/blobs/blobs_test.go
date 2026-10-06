package blobs

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
	"github.com/SkYNewZ/sos-vpdive/internal/secure"
)

func newKey(t *testing.T) string {
	t.Helper()
	k, err := secure.NewToken()
	require.NoError(t, err)
	return k
}

// exercise runs the same checks against any Store.
func exercise(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	k1, k2 := newKey(t), newKey(t)

	_, err := s.Get(ctx, k1)
	require.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, s.Put(ctx, k1, []byte("sealed one")))
	require.NoError(t, s.Put(ctx, k2, []byte("sealed two")))
	got, err := s.Get(ctx, k1)
	require.NoError(t, err)
	assert.Equal(t, []byte("sealed one"), got)

	list, err := s.List(ctx)
	require.NoError(t, err)
	keys := make([]string, 0, len(list))
	for _, o := range list {
		keys = append(keys, o.Key)
		assert.WithinDuration(t, time.Now(), o.Modified, time.Minute)
	}
	assert.ElementsMatch(t, []string{k1, k2}, keys)

	require.NoError(t, s.Delete(ctx, k1))
	require.NoError(t, s.Delete(ctx, k1), "deleting a missing key is not an error")
	_, err = s.Get(ctx, k1)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestDir(t *testing.T) {
	d, err := NewDir(t.TempDir())
	require.NoError(t, err)
	exercise(t, d)
}

func TestDirKeepsKeysInsideTheDirectory(t *testing.T) {
	d, err := NewDir(t.TempDir() + "/captures")
	require.NoError(t, err)
	ctx := context.Background()
	require.Error(t, d.Put(ctx, "../escape", []byte("x")))
	_, err = d.Get(ctx, "../escape")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNotFound)
}

// fakeS3 implements the four calls the store makes, path-style, on one bucket.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	paths   []string
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	key, isObject := strings.CutPrefix(r.URL.Path, "/captures/")
	isObject = isObject && key != ""
	switch {
	case r.Method == http.MethodPut && isObject:
		body, _ := io.ReadAll(r.Body)
		f.objects[key] = body
	case r.Method == http.MethodGet && isObject:
		body, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>no</Message></Error>`)
			return
		}
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		_, _ = w.Write(body)
	case r.Method == http.MethodDelete && isObject:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && strings.TrimSuffix(r.URL.Path, "/") == "/captures" && r.URL.Query().Get("list-type") == "2":
		type content struct {
			Key          string `xml:"Key"`
			LastModified string `xml:"LastModified"`
			Size         int    `xml:"Size"`
		}
		res := struct {
			XMLName     xml.Name  `xml:"ListBucketResult"`
			Name        string    `xml:"Name"`
			IsTruncated bool      `xml:"IsTruncated"`
			Contents    []content `xml:"Contents"`
		}{Name: "captures"}
		for k, v := range f.objects {
			res.Contents = append(res.Contents, content{Key: k, LastModified: time.Now().UTC().Format(time.RFC3339), Size: len(v)})
		}
		_ = xml.NewEncoder(w).Encode(res)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func TestS3(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewTLSServer(fake) // https, as R2: payloads are sent unchunked
	t.Cleanup(srv.Close)
	endpoint, err := url.Parse(srv.URL)
	require.NoError(t, err)

	s, err := newS3(config.S3{Endpoint: endpoint, Bucket: "captures", AccessKeyID: "id", SecretAccessKey: "secret", Region: "auto"},
		srv.Client().Transport)
	require.NoError(t, err)
	exercise(t, s)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, p := range fake.paths {
		assert.True(t, strings.HasPrefix(p, "PUT /captures/") || strings.HasPrefix(p, "GET /captures") ||
			strings.HasPrefix(p, "DELETE /captures/"), "path-style call on the bucket only: %s", p)
	}
}

func TestS3UnreachableIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	endpoint, err := url.Parse(srv.URL)
	require.NoError(t, err)
	srv.Close()

	s, err := newS3(config.S3{Endpoint: endpoint, Bucket: "captures", AccessKeyID: "id", SecretAccessKey: "secret", Region: "auto"}, nil)
	require.NoError(t, err)
	err = s.Put(context.Background(), newKey(t), []byte("x"))
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotFound)
}

func TestNewPicksTheStore(t *testing.T) {
	dir := t.TempDir()
	s, err := New(&config.Config{DataDir: dir})
	require.NoError(t, err)
	assert.IsType(t, &Dir{}, s)

	endpoint, err := url.Parse("https://account.eu.r2.cloudflarestorage.com")
	require.NoError(t, err)
	s, err = New(&config.Config{DataDir: dir, S3: &config.S3{Endpoint: endpoint, Bucket: "b", AccessKeyID: "i", SecretAccessKey: "s", Region: "auto"}})
	require.NoError(t, err)
	assert.IsType(t, &S3{}, s)
}

func TestErrorsCarryNoObjectKey(t *testing.T) {
	ctx := context.Background()
	key := newKey(t)

	srv := httptest.NewServer(http.NotFoundHandler())
	endpoint, err := url.Parse(srv.URL)
	require.NoError(t, err)
	srv.Close()
	s3, err := newS3(config.S3{Endpoint: endpoint, Bucket: "captures", AccessKeyID: "id", SecretAccessKey: "secret", Region: "auto"}, nil)
	require.NoError(t, err)

	// A directory in place of the object makes Remove fail with a *fs.PathError.
	dirPath := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dirPath, key, "inner"), 0o700))
	dir, err := NewDir(dirPath)
	require.NoError(t, err)

	for name, s := range map[string]Store{"s3": s3, "dir": dir} {
		_, getErr := s.Get(ctx, key)
		for op, err := range map[string]error{
			"put": s.Put(ctx, key, []byte("x")), "get": getErr, "delete": s.Delete(ctx, key),
		} {
			if err != nil {
				assert.NotContains(t, err.Error(), key, "%s %s", name, op)
			}
		}
	}
	require.Error(t, dir.Delete(ctx, key), "the directory case must fail")
	require.Error(t, s3.Delete(ctx, key))
}
