//go:build integration

package certmagicoss_test

import (
	"context"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/google/tink/go/aead"
	"github.com/google/tink/go/keyset"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	osstorage "github.com/aUsernameWoW/certmagic-oss/storage"
)

// These tests run against a real Alibaba Cloud OSS bucket.
// They are gated behind the "integration" build tag so they
// don't run in CI or during normal `go test`.
//
// Run with:
//   go test -tags integration -v -count=1 .
//
// Required environment variables (or defaults from Caddy config):
//   OSS_BUCKET, OSS_REGION, OSS_ENDPOINT, OSS_ACCESS_KEY_ID, OSS_ACCESS_KEY_SECRET
//
// All test objects are stored under the "certmagic-oss-test/" prefix
// and cleaned up after each test.

const testPrefix = "certmagic-oss-test/"

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newIntegrationStorage(t *testing.T) *osstorage.Storage {
	t.Helper()

	bucket := os.Getenv("OSS_BUCKET")
	region := os.Getenv("OSS_REGION")
	endpoint := os.Getenv("OSS_ENDPOINT")
	accessKeyID := os.Getenv("OSS_ACCESS_KEY_ID")
	accessKeySecret := os.Getenv("OSS_ACCESS_KEY_SECRET")

	if bucket == "" || accessKeyID == "" || accessKeySecret == "" {
		t.Skip("Skipping integration test: set OSS_BUCKET, OSS_REGION, OSS_ENDPOINT, OSS_ACCESS_KEY_ID, OSS_ACCESS_KEY_SECRET")
	}
	if region == "" {
		region = "cn-hongkong"
	}
	if endpoint == "" {
		endpoint = "oss-" + region + ".aliyuncs.com"
	}

	s, err := osstorage.NewStorage(context.Background(), osstorage.Config{
		BucketName:      bucket,
		Region:          region,
		Endpoint:        endpoint,
		AccessKeyID:     accessKeyID,
		AccessKeySecret: accessKeySecret,
	})
	require.NoError(t, err)
	return s
}

// cleanup deletes all objects under the test prefix.
func cleanup(t *testing.T, s *osstorage.Storage) {
	t.Helper()
	ctx := context.Background()
	keys, err := s.List(ctx, testPrefix, true)
	if err != nil {
		t.Logf("cleanup list error (non-fatal): %v", err)
		return
	}
	for _, key := range keys {
		_ = s.Delete(ctx, key)
	}
}

func TestIntegration_CRUD(t *testing.T) {
	s := newIntegrationStorage(t)
	t.Cleanup(func() { cleanup(t, s) })
	ctx := context.Background()

	key := testPrefix + "test-crud/cert.pem"
	content := []byte("-----BEGIN CERTIFICATE-----\nIntegrationTestData\n-----END CERTIFICATE-----")

	// Should not exist initially
	assert.False(t, s.Exists(ctx, key))

	// Store
	require.NoError(t, s.Store(ctx, key, content))

	// Exists
	assert.True(t, s.Exists(ctx, key))

	// Load
	loaded, err := s.Load(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, content, loaded)

	// Stat
	info, err := s.Stat(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, key, info.Key)
	assert.Equal(t, int64(len(content)), info.Size)
	assert.True(t, info.IsTerminal)
	assert.False(t, info.Modified.IsZero())

	// Overwrite
	newContent := []byte("updated cert data")
	require.NoError(t, s.Store(ctx, key, newContent))
	loaded, err = s.Load(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, newContent, loaded)

	// Delete
	require.NoError(t, s.Delete(ctx, key))
	assert.False(t, s.Exists(ctx, key))

	// Load after delete
	_, err = s.Load(ctx, key)
	assert.ErrorIs(t, err, fs.ErrNotExist)
}

func TestIntegration_List(t *testing.T) {
	s := newIntegrationStorage(t)
	t.Cleanup(func() { cleanup(t, s) })
	ctx := context.Background()

	keys := []string{
		testPrefix + "list/a/cert.pem",
		testPrefix + "list/a/key.pem",
		testPrefix + "list/b/cert.pem",
		testPrefix + "list/toplevel.txt",
	}
	for _, k := range keys {
		require.NoError(t, s.Store(ctx, k, []byte("data")))
	}

	// Recursive
	result, err := s.List(ctx, testPrefix+"list/", true)
	require.NoError(t, err)
	assert.Len(t, result, 4)

	// Non-recursive: should only return direct children
	result, err = s.List(ctx, testPrefix+"list/", false)
	require.NoError(t, err)
	for _, r := range result {
		rest := strings.TrimPrefix(r, testPrefix+"list/")
		assert.NotContains(t, rest, "/", "non-recursive should not return nested: %s", r)
	}
}

func TestIntegration_Encryption(t *testing.T) {
	s := newIntegrationStorage(t)
	t.Cleanup(func() { cleanup(t, s) })
	ctx := context.Background()

	// Create a new storage with encryption
	kh, err := keyset.NewHandle(aead.AES256GCMKeyTemplate())
	require.NoError(t, err)
	kp, err := aead.New(kh)
	require.NoError(t, err)

	encStorage, err := osstorage.NewStorage(ctx, osstorage.Config{
		BucketName:      os.Getenv("OSS_BUCKET"),
		Region:          envOrDefault("OSS_REGION", "cn-hongkong"),
		Endpoint:        envOrDefault("OSS_ENDPOINT", "oss-"+envOrDefault("OSS_REGION", "cn-hongkong")+".aliyuncs.com"),
		AccessKeyID:     os.Getenv("OSS_ACCESS_KEY_ID"),
		AccessKeySecret: os.Getenv("OSS_ACCESS_KEY_SECRET"),
		AEAD:            kp,
	})
	require.NoError(t, err)

	key := testPrefix + "encrypted/secret.key"
	secret := []byte("super-secret-private-key-data")

	require.NoError(t, encStorage.Store(ctx, key, secret))

	// Reading with encryption should return plaintext
	loaded, err := encStorage.Load(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, secret, loaded)

	// Reading with the NON-encrypted storage should get gibberish (encrypted bytes)
	raw, err := s.Load(ctx, key)
	require.NoError(t, err)
	assert.NotEqual(t, secret, raw, "encrypted data should differ from plaintext")

	// Cleanup
	_ = encStorage.Delete(ctx, key)
}

func TestIntegration_LockUnlock(t *testing.T) {
	s := newIntegrationStorage(t)
	t.Cleanup(func() { cleanup(t, s) })
	ctx := context.Background()

	lockKey := testPrefix + "locktest"

	// Lock
	require.NoError(t, s.Lock(ctx, lockKey))

	// Unlock
	require.NoError(t, s.Unlock(ctx, lockKey))
}

func TestIntegration_LockContention(t *testing.T) {
	s := newIntegrationStorage(t)
	t.Cleanup(func() { cleanup(t, s) })

	// Use shorter intervals for the test
	origPoll := osstorage.LockPollInterval
	osstorage.LockPollInterval = 500 * time.Millisecond
	defer func() { osstorage.LockPollInterval = origPoll }()

	lockKey := testPrefix + "contention"
	ctx := context.Background()

	// Acquire lock
	require.NoError(t, s.Lock(ctx, lockKey))

	// Second lock should block, then succeed after unlock
	done := make(chan error, 1)
	go func() {
		done <- s.Lock(ctx, lockKey)
	}()

	// Wait a bit, then unlock
	time.Sleep(1 * time.Second)
	require.NoError(t, s.Unlock(ctx, lockKey))

	select {
	case err := <-done:
		assert.NoError(t, err)
		// Clean up the second lock
		_ = s.Unlock(ctx, lockKey)
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for contested lock")
	}
}

func TestIntegration_CertMagicDefaultStorage(t *testing.T) {
	s := newIntegrationStorage(t)
	t.Cleanup(func() { cleanup(t, s) })

	certmagic.Default.Storage = s
	ctx := context.Background()

	key := testPrefix + "certmagic-default/test.txt"
	require.NoError(t, certmagic.Default.Storage.Store(ctx, key, []byte("certmagic works")))

	loaded, err := certmagic.Default.Storage.Load(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, []byte("certmagic works"), loaded)
}
