package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
	"github.com/caddyserver/certmagic"
	"github.com/google/tink/go/tink"
)

var (
	// LockExpiration is the duration before which a Lock is considered expired
	LockExpiration = 1 * time.Minute
	// LockPollInterval is the interval between each check of the lock state.
	LockPollInterval = 1 * time.Second
)

// Storage is a certmagic.Storage backed by an OSS bucket
type Storage struct {
	client     *oss.Client
	bucketName string
	aead       tink.AEAD
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
	// Region is the OSS region
	Region string
	// Endpoint is the OSS endpoint
	Endpoint string
	// AccessKeyID is the access key ID for OSS
	AccessKeyID string
	// AccessKeySecret is the access key secret for OSS
	AccessKeySecret string
}

func NewStorage(ctx context.Context, config Config) (*Storage, error) {
	// Create credentials provider
	creds := credentials.NewStaticCredentialsProvider(config.AccessKeyID, config.AccessKeySecret, "")
	
	// Create config
	cfg := oss.LoadDefaultConfig().
		WithCredentialsProvider(creds).
		WithRegion(config.Region)
	
	// If endpoint is specified, use it
	if config.Endpoint != "" {
		// Validate and format endpoint URL
		endpoint := config.Endpoint
		if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
			endpoint = "https://" + endpoint
		}
		cfg = cfg.WithEndpoint(endpoint)
	}
	
	// Create client
	client := oss.NewClient(cfg)
	
	var kp tink.AEAD
	if config.AEAD != nil {
		kp = config.AEAD
	} else {
		kp = new(cleartext)
	}
	
	return &Storage{client: client, bucketName: config.BucketName, aead: kp}, nil
}

// Store puts value at key.
func (s *Storage) Store(ctx context.Context, key string, value []byte) error {
	encrypted, err := s.aead.Encrypt(value, []byte(key))
	if err != nil {
		return fmt.Errorf("encrypting object %s: %w", key, err)
	}
	
	// Use the PutObject API
	_, err = s.client.PutObject(ctx, &oss.PutObjectRequest{
		Bucket: oss.Ptr(s.bucketName),
		Key:    oss.Ptr(key),
		Body:   bytes.NewReader(encrypted),
	})
	
	if err != nil {
		return fmt.Errorf("writing object %s: %w", key, err)
	}
	return nil
}

// Load retrieves the value at key.
func (s *Storage) Load(ctx context.Context, key string) ([]byte, error) {
	result, err := s.client.GetObject(ctx, &oss.GetObjectRequest{
		Bucket: oss.Ptr(s.bucketName),
		Key:    oss.Ptr(key),
	})
	
	if err != nil {
		// Check if it's a "not found" error
		var serviceErr *oss.ServiceError
		if errors.As(err, &serviceErr) && serviceErr.ErrorCode() == "NoSuchKey" {
			return nil, fs.ErrNotExist
		}
		return nil, fmt.Errorf("loading object %s: %w", key, err)
	}
	defer result.Body.Close()

	encrypted, err := io.ReadAll(result.Body)
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
	_, err := s.client.DeleteObject(ctx, &oss.DeleteObjectRequest{
		Bucket: oss.Ptr(s.bucketName),
		Key:    oss.Ptr(key),
	})
	
	if err != nil {
		// Check if it's a "not found" error
		var serviceErr *oss.ServiceError
		if errors.As(err, &serviceErr) && serviceErr.ErrorCode() == "NoSuchKey" {
			// Ignore "not found" errors
			return nil
		}
		return fmt.Errorf("deleting object %s: %w", key, err)
	}
	return nil
}

// Exists returns true if the key exists
// and there was no error checking.
func (s *Storage) Exists(ctx context.Context, key string) bool {
	_, err := s.client.HeadObject(ctx, &oss.HeadObjectRequest{
		Bucket: oss.Ptr(s.bucketName),
		Key:    oss.Ptr(key),
	})
	
	// Check if it's a "not found" error
	if err != nil {
		var serviceErr *oss.ServiceError
		if errors.As(err, &serviceErr) && serviceErr.ErrorCode() == "NoSuchKey" {
			return false
		}
		// For other errors, we assume the key doesn't exist
		return false
	}
	return true
}

// List returns all keys that match prefix.
// If recursive is true, non-terminal keys
// will be enumerated (i.e. "directories"
// should be walked); otherwise, only keys
// prefixed exactly by prefix will be listed.
func (s *Storage) List(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	var names []string
	
	// Set up options for listing objects
	request := &oss.ListObjectsV2Request{
		Bucket: oss.Ptr(s.bucketName),
		Prefix: oss.Ptr(prefix),
	}
	
	// If not recursive, we need to set delimiter to "/"
	if !recursive {
		request.Delimiter = oss.Ptr("/")
	}
	
	// Create paginator for listing objects
	p := s.client.NewListObjectsV2Paginator(request)
	
	// Iterate through the object pages
	for p.HasNext() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing objects: %w", err)
		}
		
		// Add object keys to result
		for _, object := range page.Contents {
			names = append(names, *object.Key)
		}
	}
	
	return names, nil
}

// Stat returns information about key.
func (s *Storage) Stat(ctx context.Context, key string) (certmagic.KeyInfo, error) {
	var keyInfo certmagic.KeyInfo
	
	result, err := s.client.HeadObject(ctx, &oss.HeadObjectRequest{
		Bucket: oss.Ptr(s.bucketName),
		Key:    oss.Ptr(key),
	})
	
	if err != nil {
		// Check if it's a "not found" error
		var serviceErr *oss.ServiceError
		if errors.As(err, &serviceErr) && serviceErr.ErrorCode() == "NoSuchKey" {
			return keyInfo, fs.ErrNotExist
		}
		return keyInfo, fmt.Errorf("loading attributes for %s: %w", key, err)
	}
	
	keyInfo.Key = key
	// Parse last modified time
	if result.LastModified != nil {
		keyInfo.Modified = *result.LastModified
	}
	
	// Parse size
	keyInfo.Size = result.ContentLength
	
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
		_, err := s.client.HeadObject(ctx, &oss.HeadObjectRequest{
			Bucket: oss.Ptr(s.bucketName),
			Key:    oss.Ptr(lockKey),
		})
		
		// Create the lock if it doesn't exist
		if err != nil {
			// Check if it's a "not found" error
			var serviceErr *oss.ServiceError
			if !(errors.As(err, &serviceErr) && serviceErr.ErrorCode() == "NoSuchKey") {
				// For other errors, return the error
				return fmt.Errorf("checking lock %s: %w", lockKey, err)
			}
			
			// Create lock object
			_, err := s.client.PutObject(ctx, &oss.PutObjectRequest{
				Bucket: oss.Ptr(s.bucketName),
				Key:    oss.Ptr(lockKey),
				Body:   bytes.NewReader([]byte{}),
			})
			
			if err != nil {
				return fmt.Errorf("creating %s: %w", lockKey, err)
			}
			continue
		}
		
		// TODO: Implement lock expiration logic
		// For now, we'll just wait and try again
		
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
	_, err := s.client.DeleteObject(ctx, &oss.DeleteObjectRequest{
		Bucket: oss.Ptr(s.bucketName),
		Key:    oss.Ptr(lockKey),
	})
	
	// Check if the error is "not found" to ignore it
	if err != nil {
		var serviceErr *oss.ServiceError
		if errors.As(err, &serviceErr) && serviceErr.ErrorCode() == "NoSuchKey" {
			// Ignore "not found" errors
			return nil
		}
		return fmt.Errorf("deleting lock %s: %w", lockKey, err)
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