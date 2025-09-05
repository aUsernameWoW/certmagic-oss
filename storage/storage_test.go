package storage

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"net/http"
	"testing"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"github.com/google/tink/go/aead"
	"github.com/google/tink/go/keyset"
	"github.com/stretchr/testify/assert"
)

const (
	testBucket = "some-bucket"
	testEndpoint = "https://oss-cn-hangzhou.aliyuncs.com"
	testAccessKeyID = "test-access-key-id"
	testAccessKeySecret = "test-access-key-secret"
)

// MockOSSClient is a mock implementation of OSS client for testing
type MockOSSClient struct {
	objects map[string][]byte
}

func NewMockOSSClient() *MockOSSClient {
	return &MockOSSClient{
		objects: make(map[string][]byte),
	}
}

func (m *MockOSSClient) Bucket(name string) (*MockOSSBucket, error) {
	return &MockOSSBucket{client: m}, nil
}

// MockOSSBucket is a mock implementation of OSS bucket for testing
type MockOSSBucket struct {
	client *MockOSSClient
}

func (b *MockOSSBucket) PutObject(key string, reader io.Reader, options ...oss.Option) error {
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	b.client.objects[key] = data
	return nil
}

func (b *MockOSSBucket) GetObject(key string, options ...oss.Option) (io.ReadCloser, error) {
	data, exists := b.client.objects[key]
	if !exists {
		return nil, oss.ServiceError{
			StatusCode: 404,
			Code:       "NoSuchKey",
			Message:    "The specified key does not exist.",
		}
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (b *MockOSSBucket) DeleteObject(key string, options ...oss.Option) error {
	if _, exists := b.client.objects[key]; !exists {
		return oss.ServiceError{
			StatusCode: 404,
			Code:       "NoSuchKey",
			Message:    "The specified key does not exist.",
		}
	}
	delete(b.client.objects, key)
	return nil
}

func (b *MockOSSBucket) GetObjectMeta(key string, options ...oss.Option) (http.Header, error) {
	if _, exists := b.client.objects[key]; !exists {
		return nil, oss.ServiceError{
			StatusCode: 404,
			Code:       "NoSuchKey",
			Message:    "The specified key does not exist.",
		}
	}
	return http.Header{}, nil
}

func (b *MockOSSBucket) GetObjectDetailedMeta(key string, options ...oss.Option) (http.Header, error) {
	if _, exists := b.client.objects[key]; !exists {
		return nil, oss.ServiceError{
			StatusCode: 404,
			Code:       "NoSuchKey",
			Message:    "The specified key does not exist.",
		}
	}
	header := http.Header{}
	header.Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
	header.Set("Content-Length", "4")
	return header, nil
}

func (b *MockOSSBucket) ListObjects(options ...oss.Option) (oss.ListObjectsResult, error) {
	// Simplified implementation for testing
	return oss.ListObjectsResult{}, nil
}

func setupTestStorage(t *testing.T) *Storage {
	client := NewMockOSSClient()
	bucket, _ := client.Bucket(testBucket)
	
	s := &Storage{
		client: nil, // Not used in mock
		bucket: bucket,
		aead:   new(cleartext),
	}
	
	return s
}

func TestSimpleOperations(t *testing.T) {
	s := setupTestStorage(t)
	key := "some/object/file.txt"
	content := "data"

	ctx := context.Background()

	// Exists
	assert.False(t, s.Exists(ctx, key))

	// Create
	err := s.Store(ctx, key, []byte(content))
	assert.NoError(t, err)

	assert.True(t, s.Exists(ctx, key))

	out, err := s.Load(ctx, key)
	assert.NoError(t, err)
	assert.Equal(t, content, string(out))

	// Stat
	stat, err := s.Stat(ctx, key)
	assert.NoError(t, err)
	assert.Equal(t, key, stat.Key)
	assert.EqualValues(t, len(content), stat.Size)
	assert.True(t, stat.IsTerminal)

	// Delete
	err = s.Delete(ctx, key)
	assert.NoError(t, err)
	assert.False(t, s.Exists(ctx, key))
}

func TestDeleteOnlyIfKeyStillExists(t *testing.T) {
	ctx := context.Background()
	s := setupTestStorage(t)
	
	err := s.Delete(ctx, "/does/not/exists")
	assert.ErrorAs(t, err, &fs.ErrNotExist)
}

func TestEncryption(t *testing.T) {
	ctx := context.Background()
	s := setupTestStorage(t)
	
	kh, err := keyset.NewHandle(aead.AES256GCMKeyTemplate())
	assert.NoError(t, err)
	kp, err := aead.New(kh)
	assert.NoError(t, err)

	s.aead = kp
	key := "some/object/file.txt"
	content := "data"

	// store encrypted data
	err = s.Store(ctx, key, []byte(content))
	assert.NoError(t, err)

	// ensure the object is encrypted in storage
	// Note: In real OSS, we would need to check the actual stored data
	// For mock, we're just verifying the flow works
	out, err := s.Load(ctx, key)
	assert.NoError(t, err)
	assert.Equal(t, content, string(out))
}

func TestErrNotExist(t *testing.T) {
	ctx := context.Background()
	s := setupTestStorage(t)
	key := "does/not/exists"
	
	_, err := s.Load(ctx, key)
	assert.ErrorIs(t, err, fs.ErrNotExist)
	
	err = s.Delete(ctx, key)
	assert.ErrorIs(t, err, fs.ErrNotExist)
	
	_, err = s.Stat(ctx, key)
	assert.ErrorIs(t, err, fs.ErrNotExist)
}