package certmagicoss_test

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
	"github.com/caddyserver/certmagic"
	"github.com/google/tink/go/aead"
	"github.com/google/tink/go/keyset"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	osstorage "github.com/aUsernameWoW/certmagic-oss/storage"
)

const testBucket = "e2e-test-bucket"

// --- Mock OSS Server (shared with storage tests but self-contained here) ---

type mockObject struct {
	data         []byte
	lastModified time.Time
}

func mockOSSServer(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	objects := make(map[string]*mockObject)

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		path := strings.TrimPrefix(r.URL.Path, "/")
		parts := strings.SplitN(path, "/", 2)
		key := ""
		if len(parts) >= 2 {
			key = parts[1]
		}

		switch r.Method {
		case http.MethodPut:
			if r.Header.Get("X-Oss-Forbid-Overwrite") == "true" {
				if _, exists := objects[key]; exists {
					w.WriteHeader(http.StatusConflict)
					writeXMLError(w, "FileAlreadyExists", "The object already exists.")
					return
				}
			}
			data, _ := io.ReadAll(r.Body)
			objects[key] = &mockObject{data: data, lastModified: time.Now().UTC()}
			w.WriteHeader(http.StatusOK)

		case http.MethodGet:
			if r.URL.Query().Get("list-type") == "2" {
				handleListV2(w, objects, r.URL.Query().Get("prefix"), r.URL.Query().Get("delimiter"))
				return
			}
			obj, exists := objects[key]
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				writeXMLError(w, "NoSuchKey", "The specified key does not exist.")
				return
			}
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(obj.data)))
			w.Header().Set("Last-Modified", obj.lastModified.Format(http.TimeFormat))
			w.Write(obj.data)

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

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
}

func writeXMLError(w http.ResponseWriter, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	type ossError struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}
	xml.NewEncoder(w).Encode(ossError{Code: code, Message: message})
}

type listBucketResult struct {
	XMLName  xml.Name     `xml:"ListBucketResult"`
	Name     string       `xml:"Name"`
	Prefix   string       `xml:"Prefix"`
	KeyCount int          `xml:"KeyCount"`
	MaxKeys  int          `xml:"MaxKeys"`
	Contents []listObject `xml:"Contents"`
}
type listObject struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	Size         int    `xml:"Size"`
}

func handleListV2(w http.ResponseWriter, objects map[string]*mockObject, prefix, delimiter string) {
	result := listBucketResult{Name: testBucket, Prefix: prefix, MaxKeys: 1000}
	for key, obj := range objects {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if delimiter != "" {
			rest := key[len(prefix):]
			if strings.Contains(rest, delimiter) {
				continue
			}
		}
		result.Contents = append(result.Contents, listObject{
			Key: key, LastModified: obj.lastModified.Format(time.RFC3339), Size: len(obj.data),
		})
	}
	result.KeyCount = len(result.Contents)
	w.Header().Set("Content-Type", "application/xml")
	xml.NewEncoder(w).Encode(result)
}

// newTestStorage creates a real Storage instance backed by the mock OSS server.
func newTestStorage(t *testing.T) certmagic.Storage {
	t.Helper()
	server := mockOSSServer(t)
	t.Cleanup(server.Close)

	cfg := oss.LoadDefaultConfig().
		WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test-ak", "test-sk", "")).
		WithRegion("test-region").
		WithEndpoint(server.URL).
		WithUsePathStyle(true)

	client := oss.NewClient(cfg)

	// We need access to the internal struct, but it's in the storage package.
	// Use NewStorage with the mock server's URL.
	s, err := osstorage.NewStorage(context.Background(), osstorage.Config{
		BucketName:      testBucket,
		Region:          "test-region",
		Endpoint:        server.URL,
		AccessKeyID:     "test-ak",
		AccessKeySecret: "test-sk",
	})
	require.NoError(t, err)
	_ = client // NewStorage creates its own client

	return s
}

// TestCertMagicIntegration verifies that our Storage implementation
// satisfies the certmagic.Storage interface contract end-to-end.
func TestCertMagicIntegration(t *testing.T) {
	storage := newTestStorage(t)
	ctx := context.Background()

	// 1. Store certificates
	certKey := "certificates/acme-v02.api.letsencrypt.org-directory/example.com/example.com.crt"
	certData := []byte("-----BEGIN CERTIFICATE-----\nMIIFake...\n-----END CERTIFICATE-----")
	require.NoError(t, storage.Store(ctx, certKey, certData))

	keyKey := "certificates/acme-v02.api.letsencrypt.org-directory/example.com/example.com.key"
	keyData := []byte("-----BEGIN EC PRIVATE KEY-----\nMIIFake...\n-----END EC PRIVATE KEY-----")
	require.NoError(t, storage.Store(ctx, keyKey, keyData))

	// 2. Verify they exist
	assert.True(t, storage.Exists(ctx, certKey))
	assert.True(t, storage.Exists(ctx, keyKey))

	// 3. Load and verify
	loaded, err := storage.Load(ctx, certKey)
	require.NoError(t, err)
	assert.Equal(t, certData, loaded)

	// 4. Stat
	info, err := storage.Stat(ctx, certKey)
	require.NoError(t, err)
	assert.Equal(t, certKey, info.Key)
	assert.True(t, info.IsTerminal)

	// 5. List
	keys, err := storage.List(ctx, "certificates/", true)
	require.NoError(t, err)
	assert.Len(t, keys, 2)

	// 6. Lock/Unlock cycle (certmagic does this during cert operations)
	locker, ok := storage.(certmagic.Locker)
	require.True(t, ok, "storage should implement certmagic.Locker")

	err = locker.Lock(ctx, "example.com")
	require.NoError(t, err)
	err = locker.Unlock(ctx, "example.com")
	require.NoError(t, err)

	// 7. Delete
	require.NoError(t, storage.Delete(ctx, certKey))
	assert.False(t, storage.Exists(ctx, certKey))

	// 8. Load deleted key should fail
	_, err = storage.Load(ctx, certKey)
	assert.ErrorIs(t, err, fs.ErrNotExist)
}

// TestCertMagicWithEncryption tests the full flow with client-side encryption.
func TestCertMagicWithEncryption(t *testing.T) {
	server := mockOSSServer(t)
	t.Cleanup(server.Close)

	// Create encryption key
	kh, err := keyset.NewHandle(aead.AES256GCMKeyTemplate())
	require.NoError(t, err)
	kp, err := aead.New(kh)
	require.NoError(t, err)

	storage, err := osstorage.NewStorage(context.Background(), osstorage.Config{
		BucketName:      testBucket,
		Region:          "test-region",
		Endpoint:        server.URL,
		AccessKeyID:     "test-ak",
		AccessKeySecret: "test-sk",
		AEAD:            kp,
	})
	require.NoError(t, err)

	ctx := context.Background()
	key := "certificates/encrypted/example.com.key"
	secret := []byte("-----BEGIN EC PRIVATE KEY-----\nSuperSecretKey\n-----END EC PRIVATE KEY-----")

	// Store encrypted
	require.NoError(t, storage.Store(ctx, key, secret))

	// Load and verify round-trip
	loaded, err := storage.Load(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, secret, loaded)
}

// TestDefaultStorageAssignment verifies that our storage can be assigned
// to certmagic.Default.Storage (the most common integration point).
func TestDefaultStorageAssignment(t *testing.T) {
	storage := newTestStorage(t)

	// This is how users typically configure it
	certmagic.Default.Storage = storage

	// Verify it works through the default config
	ctx := context.Background()
	key := "test/assignment/key"
	require.NoError(t, certmagic.Default.Storage.Store(ctx, key, []byte("works")))

	loaded, err := certmagic.Default.Storage.Load(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, []byte("works"), loaded)
}
