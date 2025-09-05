package certmagicoss

import (
	"context"
	"fmt"
	"os"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/certmagic"
	"github.com/google/tink/go/aead"
	"github.com/google/tink/go/insecurecleartextkeyset"
	"github.com/google/tink/go/keyset"
	"github.com/aUsernameWoW/certmagic-oss/storage"
)

// Interface guards
var (
	_ caddyfile.Unmarshaler  = (*CaddyStorageOSS)(nil)
	_ caddy.StorageConverter = (*CaddyStorageOSS)(nil)
)

// CaddyStorageOSS implements a caddy storage backend for Alibaba Cloud OSS.
type CaddyStorageOSS struct {
	// BucketName is the name of the storage bucket.
	BucketName string `json:"bucket-name"`
	// Region is the OSS region.
	Region string `json:"region"`
	// Endpoint is the OSS endpoint.
	Endpoint string `json:"endpoint"`
	// AccessKeyID is the access key ID for OSS.
	AccessKeyID string `json:"access-key-id"`
	// AccessKeySecret is the access key secret for OSS.
	AccessKeySecret string `json:"access-key-secret"`
	// EncryptionKeySet is the path of a json tink encryption keyset
	EncryptionKeySet string `json:"encryption-key-set"`
}

func init() {
	caddy.RegisterModule(CaddyStorageOSS{})
}

// CaddyModule returns the Caddy module information.
func (CaddyStorageOSS) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID: "caddy.storage.oss",
		New: func() caddy.Module {
			return new(CaddyStorageOSS)
		},
	}
}

// CertMagicStorage returns a cert-magic storage.
func (s *CaddyStorageOSS) CertMagicStorage() (certmagic.Storage, error) {
	config := storage.Config{
		BucketName:      s.BucketName,
		Region:          s.Region,
		Endpoint:        s.Endpoint,
		AccessKeyID:     s.AccessKeyID,
		AccessKeySecret: s.AccessKeySecret,
	}

	if len(s.EncryptionKeySet) > 0 {
		f, err := os.Open(s.EncryptionKeySet)
		if err != nil {
			return nil, err
		}
		defer f.Close()

		r := keyset.NewJSONReader(f)
		// TODO: Add the ability to read an encrypted keyset / or envelope encryption
		// see https://github.com/google/tink/blob/e5c9356ed471be08a63eb5ea3ad0e892544e5a1c/go/keyset/handle_test.go#L84-L86
		// or https://github.com/google/tink/blob/master/docs/GOLANG-HOWTO.md
		kh, err := insecurecleartextkeyset.Read(r)
		if err != nil {
			return nil, err
		}
		kp, err := aead.New(kh)
		if err != nil {
			return nil, err
		}
		config.AEAD = kp
	}
	return storage.NewStorage(context.Background(), config)
}

// Validate caddy oss storage configuration.
func (s *CaddyStorageOSS) Validate() error {
	if s.BucketName == "" {
		return fmt.Errorf("bucket name must be defined")
	}
	if s.Region == "" {
		return fmt.Errorf("region must be defined")
	}
	if s.AccessKeyID == "" {
		return fmt.Errorf("access key id must be defined")
	}
	if s.AccessKeySecret == "" {
		return fmt.Errorf("access key secret must be defined")
	}
	return nil
}

// UnmarshalCaddyfile unmarshall caddy file.
func (s *CaddyStorageOSS) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		key := d.Val()
		var value string

		if !d.Args(&value) {
			continue
		}

		switch key {
		case "bucket-name":
			s.BucketName = value
		case "region":
			s.Region = value
		case "endpoint":
			s.Endpoint = value
		case "access-key-id":
			s.AccessKeyID = value
		case "access-key-secret":
			s.AccessKeySecret = value
		case "encryption-key-set":
			s.EncryptionKeySet = value
		}
	}
	return nil
}