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
		// Try to create the lock object atomically using ForbidOverwrite header
		// This will only succeed if the object doesn't already exist
		_, err := s.client.PutObject(ctx, &oss.PutObjectRequest{
			Bucket: oss.Ptr(s.bucketName),
			Key:    oss.Ptr(lockKey),
			Body:   bytes.NewReader([]byte{}),
			ForbidOverwrite: oss.Ptr("true"), // This ensures the object is only created if it doesn't exist
		})
		
		// If we successfully created the lock, return
		if err == nil {
			return nil
		}
		
		// Check if the error is because the lock already exists
		var serviceErr *oss.ServiceError
		if errors.As(err, &serviceErr) && (serviceErr.ErrorCode() == "PreconditionFailed" || serviceErr.ErrorCode() == "ObjectAlreadyExists" || serviceErr.ErrorCode() == "FileAlreadyExists") {
			// Lock already exists, check if it has expired
			result, err := s.client.HeadObject(ctx, &oss.HeadObjectRequest{
				Bucket: oss.Ptr(s.bucketName),
				Key:    oss.Ptr(lockKey),
			})
			
			if err != nil {
				// If we can't check the lock, continue polling
				select {
				case <-time.After(LockPollInterval):
					continue
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			
			// Check if the lock has expired
			if result.LastModified != nil && result.LastModified.Add(LockExpiration).Before(time.Now().UTC()) {
				// Lock has expired, try to delete it and then acquire the lock
				_, deleteErr := s.client.DeleteObject(ctx, &oss.DeleteObjectRequest{
					Bucket: oss.Ptr(s.bucketName),
					Key:    oss.Ptr(lockKey),
				})
				
				// If we successfully deleted the expired lock or if it was already deleted, try to acquire the lock again
				if deleteErr == nil || (errors.As(deleteErr, &serviceErr) && serviceErr.ErrorCode() == "NoSuchKey") {
					continue // Try to acquire the lock again
				}
				
				// If we couldn't delete the expired lock, continue polling
				select {
				case <-time.After(LockPollInterval):
					continue
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			
			// Lock exists and hasn't expired, wait and try again
			select {
			case <-time.After(LockPollInterval):
				continue // Try again
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		
		// For other errors, return the error
		return fmt.Errorf("creating lock %s: %w", lockKey, err)
	}
}

// Unlock releases the lock for key. This method must ONLY be
// called after a successful call to Lock, and only after the
// critical section is finished, even if it errored or timed
// out. Unlock cleans up any resources allocated during Lock.
func (s *Storage) Unlock(ctx context.Context, key string) error {
	lockKey := s.objLockName(key)
	
	// Delete the lock object
	// We use a background context to ensure we can delete the lock even if the original context is cancelled
	// This is important for cleanup operations
	deleteCtx := context.Background()
	_, err := s.client.DeleteObject(deleteCtx, &oss.DeleteObjectRequest{
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