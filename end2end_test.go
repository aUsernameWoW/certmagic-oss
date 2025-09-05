package certmagicoss_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"io/fs"

	"github.com/caddyserver/certmagic"
	"github.com/google/tink/go/aead"
	"github.com/google/tink/go/keyset"
	"github.com/google/tink/go/tink"
	"github.com/letsencrypt/pebble/v2/ca"
	"github.com/letsencrypt/pebble/v2/db"
	"github.com/letsencrypt/pebble/v2/va"
	"github.com/letsencrypt/pebble/v2/wfe"
	"github.com/stretchr/testify/assert"
)

const (
	testBucket = "some-bucket"
	testEndpoint = "https://oss-cn-hangzhou.aliyuncs.com"
	testAccessKeyID = "test-access-key-id"
	testAccessKeySecret = "test-access-key-secret"
)

func testLogger(t *testing.T) *log.Logger {
	return log.New(testWriter{t}, "test", log.LstdFlags)
}

type testWriter struct {
	t *testing.T
}

func (tw testWriter) Write(p []byte) (n int, err error) {
	tw.t.Log(string(p))
	return len(p), nil
}

func pebbleHandler(t *testing.T) http.Handler {
	t.Helper()
	t.Setenv("PEBBLE_VA_ALWAYS_VALID", "1")
	t.Setenv("PEBBLE_VA_NOSLEEP", "1")

	logger := testLogger(t)
	db := db.NewMemoryStore()
	ca := ca.New(logger, db, "", 0, 1, 100)
	va := va.New(logger, 80, 443, false, "", db)
	wfeImpl := wfe.New(logger, db, va, ca, false, false, 0, 0)
	return wfeImpl.Handler()
}

// MockOSSClient is a mock implementation of OSS client for testing
type MockOSSClient struct {
	objects map[string][]byte
}

func NewMockOSSClient() *MockOSSClient {
	return &MockOSSClient{
		objects: make(map[string][]byte),
	}
}

// MockOSSBucket is a mock implementation of OSS bucket for testing
type MockOSSBucket struct {
	client *MockOSSClient
}

func (m *MockOSSClient) Bucket(name string) (*MockOSSBucket, error) {
	return &MockOSSBucket{client: m}, nil
}

func (b *MockOSSBucket) PutObject(key string, reader io.Reader) error {
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	b.client.objects[key] = data
	return nil
}

func (b *MockOSSBucket) GetObject(key string) (io.ReadCloser, error) {
	data, exists := b.client.objects[key]
	if !exists {
		return nil, fmt.Errorf("object not found")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (b *MockOSSBucket) DeleteObject(key string) error {
	delete(b.client.objects, key)
	return nil
}

func (b *MockOSSBucket) GetObjectMeta(key string) (map[string][]string, error) {
	if _, exists := b.client.objects[key]; !exists {
		return nil, fmt.Errorf("object not found")
	}
	return map[string][]string{}, nil
}

func (b *MockOSSBucket) GetObjectDetailedMeta(key string) (map[string]string, error) {
	if _, exists := b.client.objects[key]; !exists {
		return nil, fmt.Errorf("object not found")
	}
	return map[string]string{
		"Last-Modified": "Mon, 02 Jan 2006 15:04:05 GMT",
		"Content-Length": "4",
	}, nil
}

	// MockStorage implements certmagic.Storage for testing
	type MockStorage struct {
		client *MockOSSClient
		bucket *MockOSSBucket
		aead   tink.AEAD
	}

func NewMockStorage() (*MockStorage, error) {
	client := NewMockOSSClient()
	bucket, _ := client.Bucket(testBucket)
	
	return &MockStorage{
		client: client,
		bucket: bucket,
		aead:   new(cleartext),
	}, nil
}

func (s *MockStorage) Store(ctx context.Context, key string, value []byte) error {
	return s.bucket.PutObject(key, bytes.NewReader(value))
}

func (s *MockStorage) Load(ctx context.Context, key string) ([]byte, error) {
	reader, err := s.bucket.GetObject(key)
	if err != nil {
		return nil, fs.ErrNotExist
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func (s *MockStorage) Delete(ctx context.Context, key string) error {
	return s.bucket.DeleteObject(key)
}

func (s *MockStorage) Exists(ctx context.Context, key string) bool {
	_, err := s.bucket.GetObjectMeta(key)
	return err == nil
}

func (s *MockStorage) List(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	// Simplified implementation for testing
	return []string{}, nil
}

func (s *MockStorage) Stat(ctx context.Context, key string) (certmagic.KeyInfo, error) {
	var keyInfo certmagic.KeyInfo
	
	_, err := s.bucket.GetObjectMeta(key)
	if err != nil {
		return keyInfo, fs.ErrNotExist
	}
	
	keyInfo.Key = key
	keyInfo.Modified = time.Now()
	keyInfo.Size = 4
	keyInfo.IsTerminal = true
	return keyInfo, nil
}

func (s *MockStorage) Lock(ctx context.Context, key string) error {
	// Simplified implementation for testing
	return nil
}

func (s *MockStorage) Unlock(ctx context.Context, key string) error {
	// Simplified implementation for testing
	return nil
}

// cleartext implements tink.AEAD interface, but simply store the object in plaintext
type cleartext struct{}

// encrypt returns the unencrypted plaintext data.
func (cleartext) Encrypt(plaintext, _ []byte) ([]byte, error) {
	return plaintext, nil
}

// decrypt returns the ciphertext as plaintext
func (cleartext) Decrypt(ciphertext, _ []byte) ([]byte, error) {
	return ciphertext, nil
}

func TestOSSStorage(t *testing.T) {
	// start let's encrypt
	pebble := httptest.NewTLSServer(pebbleHandler(t))
	defer pebble.Close()

	// Setup cert-magic
	certmagic.DefaultACME.CA = pebble.URL + "/dir"
	certmagic.DefaultACME.AltTLSALPNPort = 8443
	
	kh, err := keyset.NewHandle(aead.AES256GCMKeyTemplate())
	assert.NoError(t, err)
	_, err = aead.New(kh)
	assert.NoError(t, err)
	
	// Use mock storage instead of real OSS for testing
	storage, err := NewMockStorage()
	assert.NoError(t, err)

	certmagic.Default.Storage = storage
	// Configure  cert pool
	pool := x509.NewCertPool()
	pool.AddCert(pebble.Certificate())
	certmagic.DefaultACME.TrustedRoots = pool

	certmagic.DefaultACME.ListenHost = "127.0.0.1"
}