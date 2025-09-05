package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"github.com/caddyserver/certmagic"
	"github.com/google/tink/go/tink"
)

var (
	// LockExpiration is the duration before which a Lock is considered expired
	LockExpiration = 1 * time.Minute
	// LockPollInterval is the interval between each check of the lock state.
	LockPollInterval = 1 * time.Second
)

// OSSBucket represents the interface for OSS bucket operations
type OSSBucket interface {
	PutObject(key string, reader io.Reader, options ...oss.Option) error
	GetObject(key string, options ...oss.Option) (io.ReadCloser, error)
	DeleteObject(key string, options ...oss.Option) error
	GetObjectMeta(key string, options ...oss.Option) (http.Header, error)
	GetObjectDetailedMeta(key string, options ...oss.Option) (http.Header, error)
	ListObjects(options ...oss.Option) (oss.ListObjectsResult, error)
}

// Storage is a certmagic.Storage backed by an OSS bucket
type Storage struct {
	client *oss.Client
	bucket OSSBucket
	aead   tink.AEAD
}

// Interface guards
var (
	_ certmagic.Storage = (*Storage)(nil)
	_ certmagic.Locker  = (*Storage)(nil)
)

type Config struct {
	// AEAD for Authenticated Encryption with Additional Data
	AEAD tink.AEAD
	// BucketName is the name of the OSS storage Bucket
	BucketName string
	// Endpoint is the OSS endpoint
	Endpoint string
	// AccessKeyID is the access key ID for OSS
	AccessKeyID string
	// AccessKeySecret is the access key secret for OSS
	AccessKeySecret string
}

func NewStorage(ctx context.Context, config Config) (*Storage, error) {
	client, err := oss.New(config.Endpoint, config.AccessKeyID, config.AccessKeySecret)
	if err != nil {
		return nil, fmt.Errorf("could not initialize OSS client: %w", err)
	}
	
	bucket, err := client.Bucket(config.BucketName)
	if err != nil {
		return nil, fmt.Errorf("could not get bucket %s: %w", config.BucketName, err)
	}
	
	var kp tink.AEAD
	if config.AEAD != nil {
		kp = config.AEAD
	} else {
		kp = new(cleartext)
	}
	
	return &Storage{client: client, bucket: OSSBucket(bucket), aead: kp}, nil
}

// Store puts value at key.
func (s *Storage) Store(ctx context.Context, key string, value []byte) error {
	encrypted, err := s.aead.Encrypt(value, []byte(key))
	if err != nil {
		return fmt.Errorf("encrypting object %s: %w", key, err)
	}
	
	// OSS doesn't have a direct context support in this SDK, but we can use the context for cancellation
	// if needed in future implementations
	err = s.bucket.PutObject(key, bytes.NewReader(encrypted))
	if err != nil {
		return fmt.Errorf("writing object %s: %w", key, err)
	}
	return nil
}

// Load retrieves the value at key.
func (s *Storage) Load(ctx context.Context, key string) ([]byte, error) {
	body, err := s.bucket.GetObject(key)
	if err != nil {
		// Check if it's a "not found" error
		if ossErr, ok := err.(oss.ServiceError); ok && ossErr.StatusCode == 404 {
			return nil, fs.ErrNotExist
		}
		return nil, fmt.Errorf("loading object %s: %w", key, err)
	}
	defer body.Close()

	encrypted, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("reading object %s: %w", key, err)
	}

	decrypted, err := s.aead.Decrypt(encrypted, []byte(key))
	if err != nil {
		return nil, fmt.Errorf("decrypting object %s: %w", key, err)
	}
	return decrypted, nil
}

// Delete deletes key. An error should be
// returned only if the key still exists
// when the method returns.
func (s *Storage) Delete(ctx context.Context, key string) error {
	err := s.bucket.DeleteObject(key)
	if err != nil {
		// Check if it's a "not found" error
		if ossErr, ok := err.(oss.ServiceError); ok && ossErr.StatusCode == 404 {
			return fs.ErrNotExist
		}
		return fmt.Errorf("deleting object %s: %w", key, err)
	}
	return nil
}

// Exists returns true if the key exists
// and there was no error checking.
func (s *Storage) Exists(ctx context.Context, key string) bool {
	_, err := s.bucket.GetObjectMeta(key)
	return err == nil
}

// List returns all keys that match prefix.
// If recursive is true, non-terminal keys
// will be enumerated (i.e. "directories"
// should be walked); otherwise, only keys
// prefixed exactly by prefix will be listed.
func (s *Storage) List(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	var names []string
	
	// Set up options for listing objects
	options := []oss.Option{
		oss.Prefix(prefix),
	}
	
	// If not recursive, we need to set delimiter to "/"
	if !recursive {
		options = append(options, oss.Delimiter("/"))
	}
	
	marker := ""
	for {
		// List objects with pagination
		lor, err := s.bucket.ListObjects(append(options, oss.Marker(marker))...)
		if err != nil {
			return nil, fmt.Errorf("listing objects: %w", err)
		}
		
		// Add object keys to result
		for _, object := range lor.Objects {
			names = append(names, object.Key)
		}
		
		// If no more objects, break
		if !lor.IsTruncated {
			break
		}
		
		// Set marker for next iteration
		marker = lor.NextMarker
	}
	
	return names, nil
}

// Stat returns information about key.
func (s *Storage) Stat(ctx context.Context, key string) (certmagic.KeyInfo, error) {
	var keyInfo certmagic.KeyInfo
	
	props, err := s.bucket.GetObjectDetailedMeta(key)
	if err != nil {
		// Check if it's a "not found" error
		if ossErr, ok := err.(oss.ServiceError); ok && ossErr.StatusCode == 404 {
			return keyInfo, fs.ErrNotExist
		}
		return keyInfo, fmt.Errorf("loading attributes for %s: %w", key, err)
	}
	
	keyInfo.Key = key
	// Parse last modified time
	if lastModified, err := time.Parse(time.RFC1123, props.Get("Last-Modified")); err == nil {
		keyInfo.Modified = lastModified
	}
	
	// Parse size
	if size := props.Get("Content-Length"); size != "" {
		if sizeVal, err := strconv.ParseInt(size, 10, 64); err == nil {
			keyInfo.Size = sizeVal
		}
	}
	
	keyInfo.IsTerminal = true
	return keyInfo, nil
}

// Lock acquires the lock for key, blocking until the lock
// can be obtained or an error is returned. Note that, even
// after acquiring a lock, an idempotent operation may have
// already been performed by another process that acquired
// the lock before - so always check to make sure idempotent
// operations still need to be performed after acquiring the
// lock.
//
// The actual implementation of obtaining of a lock must be
// an atomic operation so that multiple Lock calls at the
// same time always results in only one caller receiving the
// lock at any given time.
//
// To prevent deadlocks, all implementations (where this concern
// is relevant) should put a reasonable expiration on the lock in
// case Unlock is unable to be called due to some sort of network
// failure or system crash. Additionally, implementations should
// honor context cancellation as much as possible (in case the
// caller wishes to give up and free resources before the lock
// can be obtained).
func (s *Storage) Lock(ctx context.Context, key string) error {
	lockKey := s.objLockName(key)
	
	for {
		// Try to get object metadata to check if lock exists
		props, err := s.bucket.GetObjectDetailedMeta(lockKey)
		
		// Create the lock if it doesn't exist
		if err != nil {
			// Check if it's a "not found" error
			if ossErr, ok := err.(oss.ServiceError); ok && ossErr.StatusCode == 404 {
				// Create lock object
				if err := s.bucket.PutObject(lockKey, bytes.NewReader([]byte{})); err != nil {
					return fmt.Errorf("creating %s: %w", lockKey, err)
				}
				continue
			} else {
				return fmt.Errorf("checking lock %s: %w", lockKey, err)
			}
		}
		
		// Acquire the lock by setting retention
		// OSS doesn't have TemporaryHold like GCS, but we can use retention policies
		// For simplicity, we'll use a marker approach - if the object exists and is not expired, it's locked
		
		// Check if lock has expired
		lastModified, err := time.Parse(time.RFC1123, props.Get("Last-Modified"))
		if err != nil {
			return fmt.Errorf("parsing lock timestamp for %s: %w", lockKey, err)
		}
		
		if lastModified.Add(LockExpiration).Before(time.Now().UTC()) {
			// Lock expired, try to unlock and recreate
			if err := s.Unlock(ctx, key); err != nil {
				return fmt.Errorf("unlocking expired lock %s: %w", lockKey, err)
			}
			continue
		}
		
		// Lock is valid, we can't acquire it now
		// Wait and try again
		select {
		case <-time.After(LockPollInterval):
			continue // a no-op since it's at the end of the loop, but nice to be explicit
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Unlock releases the lock for key. This method must ONLY be
// called after a successful call to Lock, and only after the
// critical section is finished, even if it errored or timed
// out. Unlock cleans up any resources allocated during Lock.
func (s *Storage) Unlock(ctx context.Context, key string) error {
	lockKey := s.objLockName(key)
	
	// Delete the lock object
	err := s.bucket.DeleteObject(lockKey)
	if err != nil {
		// If object doesn't exist, that's fine
		if ossErr, ok := err.(oss.ServiceError); !ok || ossErr.StatusCode != 404 {
			return fmt.Errorf("deleting lock %s: %w", lockKey, err)
		}
	}
	
	return nil
}

func (s *Storage) objLockName(key string) string {
	return key + ".lock"
}

// cleartext implements tink.AAED interface, but simply store the object in plaintext
type cleartext struct{}

// encrypt returns the unencrypted plaintext data.
func (cleartext) Encrypt(plaintext, _ []byte) ([]byte, error) {
	return plaintext, nil
}

// decrypt returns the ciphertext as plaintext
func (cleartext) Decrypt(ciphertext, _ []byte) ([]byte, error) {
	return ciphertext, nil
}