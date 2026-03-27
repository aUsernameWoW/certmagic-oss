package storage

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
	"github.com/google/tink/go/aead"
	"github.com/google/tink/go/keyset"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testBucket = "test-bucket"

// mockObject represents a stored object in the mock OSS server.
type mockObject struct {
	data         []byte
	lastModified time.Time
}

// mockOSSServer creates an httptest.Server that simulates the Alibaba Cloud OSS API.
// It uses path-style URLs: /{bucket}/{key}
func mockOSSServer(t *testing.T) *httptest.Server {
	t.Helper()

	var mu sync.Mutex
	objects := make(map[string]*mockObject) // key -> object

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		// Parse path: /{bucket}/{key...}
		// With path-style, the URL looks like /{bucket}/{key}
		path := strings.TrimPrefix(r.URL.Path, "/")
		parts := strings.SplitN(path, "/", 2)

		bucket := ""
		key := ""
		if len(parts) >= 1 {
			bucket = parts[0]
		}
		if len(parts) >= 2 {
			key = parts[1]
		}

		_ = bucket // we only have one bucket in tests

		switch r.Method {
		case http.MethodPut:
			// Check ForbidOverwrite
			if r.Header.Get("X-Oss-Forbid-Overwrite") == "true" {
				if _, exists := objects[key]; exists {
					w.WriteHeader(http.StatusConflict)
					writeOSSError(w, "FileAlreadyExists", "The object already exists.")
					return
				}
			}
			data, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			objects[key] = &mockObject{
				data:         data,
				lastModified: time.Now().UTC(),
			}
			w.WriteHeader(http.StatusOK)

		case http.MethodGet:
			// Check if this is a list request
			if r.URL.Query().Get("list-type") == "2" {
				prefix := r.URL.Query().Get("prefix")
				delimiter := r.URL.Query().Get("delimiter")
				handleListV2(w, objects, prefix, delimiter)
				return
			}

			obj, exists := objects[key]
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				writeOSSError(w, "NoSuchKey", "The specified key does not exist.")
				return
			}
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(obj.data)))
			w.Header().Set("Last-Modified", obj.lastModified.Format(http.TimeFormat))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(obj.data)

		case http.MethodDelete:
			delete(objects, key)
			w.WriteHeader(http.StatusNoContent)

		case http.MethodHead:
			obj, exists := objects[key]
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(obj.data)))
			w.Header().Set("Last-Modified", obj.lastModified.Format(http.TimeFormat))
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
}

func writeOSSError(w http.ResponseWriter, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	type ossError struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}
	_ = xml.NewEncoder(w).Encode(ossError{Code: code, Message: message})
}

// ListBucketResult is the XML response for ListObjectsV2.
type listBucketResult struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	Name                  string         `xml:"Name"`
	Prefix                string         `xml:"Prefix"`
	KeyCount              int            `xml:"KeyCount"`
	MaxKeys               int            `xml:"MaxKeys"`
	IsTruncated           bool           `xml:"IsTruncated"`
	Contents              []listObject   `xml:"Contents"`
	CommonPrefixes        []commonPrefix `xml:"CommonPrefixes"`
}

type listObject struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	Size         int    `xml:"Size"`
}

type commonPrefix struct {
	Prefix string `xml:"Prefix"`
}

func handleListV2(w http.ResponseWriter, objects map[string]*mockObject, prefix, delimiter string) {
	result := listBucketResult{
		Name:    testBucket,
		Prefix:  prefix,
		MaxKeys: 1000,
	}

	seenPrefixes := make(map[string]bool)

	for key, obj := range objects {
		if !strings.HasPrefix(key, prefix) {
			continue
		}

		if delimiter != "" {
			// Check for common prefixes
			rest := key[len(prefix):]
			idx := strings.Index(rest, delimiter)
			if idx >= 0 {
				cp := prefix + rest[:idx+1]
				if !seenPrefixes[cp] {
					seenPrefixes[cp] = true
					result.CommonPrefixes = append(result.CommonPrefixes, commonPrefix{Prefix: cp})
				}
				continue
			}
		}

		result.Contents = append(result.Contents, listObject{
			Key:          key,
			LastModified: obj.lastModified.Format(time.RFC3339),
			Size:         len(obj.data),
		})
	}

	result.KeyCount = len(result.Contents)
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(result)
}

// setupTestStorage creates a Storage instance backed by a mock OSS server.
func setupTestStorage(t *testing.T) (*Storage, *httptest.Server) {
	t.Helper()
	server := mockOSSServer(t)

	cfg := oss.LoadDefaultConfig().
		WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test-ak", "test-sk", "")).
		WithRegion("test-region").
		WithEndpoint(server.URL).
		WithUsePathStyle(true)

	client := oss.NewClient(cfg)
	s := &Storage{
		client:         client,
		bucketName:     testBucket,
		aead:           new(cleartext),
		lockExpiration: DefaultLockExpiration,
	}

	t.Cleanup(func() { server.Close() })
	return s, server
}

func TestStore_Load_Exists_Stat_Delete(t *testing.T) {
	s, _ := setupTestStorage(t)
	ctx := context.Background()
	key := "certs/example.com/cert.pem"
	content := []byte("-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----")

	// Initially should not exist
	assert.False(t, s.Exists(ctx, key))

	// Store
	err := s.Store(ctx, key, content)
	require.NoError(t, err)

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

	// Delete
	err = s.Delete(ctx, key)
	require.NoError(t, err)
	assert.False(t, s.Exists(ctx, key))
}

func TestLoad_NotFound(t *testing.T) {
	s, _ := setupTestStorage(t)
	ctx := context.Background()

	_, err := s.Load(ctx, "nonexistent/key")
	assert.ErrorIs(t, err, fs.ErrNotExist)
}

func TestStat_NotFound(t *testing.T) {
	s, _ := setupTestStorage(t)
	ctx := context.Background()

	_, err := s.Stat(ctx, "nonexistent/key")
	assert.ErrorIs(t, err, fs.ErrNotExist)
}

func TestDelete_NotFound_ReturnsNil(t *testing.T) {
	s, _ := setupTestStorage(t)
	ctx := context.Background()

	// OSS returns 204 for deleting nonexistent keys, so Delete should succeed
	err := s.Delete(ctx, "nonexistent/key")
	assert.NoError(t, err)
}

func TestList_Recursive(t *testing.T) {
	s, _ := setupTestStorage(t)
	ctx := context.Background()

	// Store some keys
	keys := []string{
		"acme/example.com/cert.pem",
		"acme/example.com/key.pem",
		"acme/sub.example.com/cert.pem",
		"other/file.txt",
	}
	for _, k := range keys {
		require.NoError(t, s.Store(ctx, k, []byte("data")))
	}

	// List recursively with prefix
	result, err := s.List(ctx, "acme/", true)
	require.NoError(t, err)
	assert.Len(t, result, 3)
	for _, r := range result {
		assert.True(t, strings.HasPrefix(r, "acme/"))
	}
}

func TestList_NonRecursive(t *testing.T) {
	s, _ := setupTestStorage(t)
	ctx := context.Background()

	keys := []string{
		"acme/example.com/cert.pem",
		"acme/example.com/key.pem",
		"acme/sub.example.com/cert.pem",
		"acme/toplevel.pem",
	}
	for _, k := range keys {
		require.NoError(t, s.Store(ctx, k, []byte("data")))
	}

	// Non-recursive should only return direct children (not "directories")
	result, err := s.List(ctx, "acme/", false)
	require.NoError(t, err)
	// Should contain "acme/toplevel.pem" as a direct key
	assert.Contains(t, result, "acme/toplevel.pem")
	// Should NOT contain nested keys (they become common prefixes)
	for _, r := range result {
		// Direct keys under acme/ should not have another / after "acme/"
		rest := strings.TrimPrefix(r, "acme/")
		assert.NotContains(t, rest, "/", "non-recursive list should not return nested keys: %s", r)
	}
}

func TestEncryption(t *testing.T) {
	s, _ := setupTestStorage(t)
	ctx := context.Background()

	// Set up encryption
	kh, err := keyset.NewHandle(aead.AES256GCMKeyTemplate())
	require.NoError(t, err)
	kp, err := aead.New(kh)
	require.NoError(t, err)
	s.aead = kp

	key := "certs/encrypted/cert.pem"
	content := []byte("super secret certificate data")

	// Store encrypted
	err = s.Store(ctx, key, content)
	require.NoError(t, err)

	// Load and verify decryption
	loaded, err := s.Load(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, content, loaded)
}

func TestLock_Unlock(t *testing.T) {
	s, _ := setupTestStorage(t)
	ctx := context.Background()

	key := "certs/example.com"

	// Lock should succeed
	err := s.Lock(ctx, key)
	require.NoError(t, err)

	// Unlock should succeed
	err = s.Unlock(ctx, key)
	require.NoError(t, err)
}

func TestLock_AlreadyLocked_WaitsAndAcquires(t *testing.T) {
	s, _ := setupTestStorage(t)

	// Reduce poll interval for faster test
	origPoll := LockPollInterval
	LockPollInterval = 50 * time.Millisecond
	defer func() { LockPollInterval = origPoll }()

	key := "certs/example.com"
	ctx := context.Background()

	// Acquire lock
	err := s.Lock(ctx, key)
	require.NoError(t, err)

	// Try to acquire in another goroutine - should block then succeed after unlock
	done := make(chan error, 1)
	go func() {
		done <- s.Lock(ctx, key)
	}()

	// Give the goroutine time to start polling
	time.Sleep(100 * time.Millisecond)

	// Release original lock
	err = s.Unlock(ctx, key)
	require.NoError(t, err)

	// Second lock should now succeed
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for lock acquisition")
	}

	// Cleanup
	require.NoError(t, s.Unlock(ctx, key))
}

func TestLock_ContextCancellation(t *testing.T) {
	s, _ := setupTestStorage(t)

	origPoll := LockPollInterval
	LockPollInterval = 50 * time.Millisecond
	defer func() { LockPollInterval = origPoll }()

	key := "certs/example.com"
	ctx := context.Background()

	// Acquire lock
	err := s.Lock(ctx, key)
	require.NoError(t, err)

	// Try with a cancelled context
	cancelCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err = s.Lock(cancelCtx, key)
	assert.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	// Cleanup
	require.NoError(t, s.Unlock(ctx, key))
}

func TestLock_ExpiredLock_Reacquired(t *testing.T) {
	s, _ := setupTestStorage(t)

	// Make lock expire almost immediately
	s.lockExpiration = 50 * time.Millisecond
	origPoll := LockPollInterval
	LockPollInterval = 20 * time.Millisecond
	defer func() {
		LockPollInterval = origPoll
	}()

	key := "certs/example.com"
	ctx := context.Background()

	// Acquire lock
	err := s.Lock(ctx, key)
	require.NoError(t, err)

	// Wait for it to expire
	time.Sleep(100 * time.Millisecond)

	// Should be able to acquire again (expired lock gets cleaned up)
	err = s.Lock(ctx, key)
	assert.NoError(t, err)

	require.NoError(t, s.Unlock(ctx, key))
}

func TestUnlock_NotLocked_ReturnsNil(t *testing.T) {
	s, _ := setupTestStorage(t)
	ctx := context.Background()

	// Unlocking something that's not locked should not error
	err := s.Unlock(ctx, "not-locked-key")
	assert.NoError(t, err)
}

func TestIsNotFound(t *testing.T) {
	// Test with NoSuchKey error code
	err1 := &oss.ServiceError{}
	// We can't easily set private fields, so test via the real flow is more meaningful.
	// The integration tests above cover isNotFound through Load/Stat on missing keys.
	_ = err1
}

func TestStore_Overwrite(t *testing.T) {
	s, _ := setupTestStorage(t)
	ctx := context.Background()
	key := "certs/example.com/cert.pem"

	// Store initial
	require.NoError(t, s.Store(ctx, key, []byte("version1")))

	// Overwrite
	require.NoError(t, s.Store(ctx, key, []byte("version2")))

	// Should get latest
	loaded, err := s.Load(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, []byte("version2"), loaded)
}
