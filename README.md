# Certmagic Storage Backend for Alibaba Cloud OSS

This library allows you to use Alibaba Cloud OSS as key/certificate storage backend for your [Certmagic](https://github.com/caddyserver/certmagic)-enabled HTTPS server. To protect your keys from unwanted attention, client-side encryption is possible.

## Usage

### Caddy

In this section, we create a caddy config using our OSS storage.

#### Getting started with Caddyfile

1. Create a `Caddyfile`
    ```
    {
      storage oss {
        bucket-name your-bucket-name
        region your-oss-region
        endpoint your-oss-endpoint
        access-key-id your-access-key-id
        access-key-secret your-access-key-secret
      }
    }
    localhost
    acme_server
    respond "Hello Caddy Storage OSS!"
    ```
   NOTE: See [https://www.alibabacloud.com/help/zh/oss/regions-and-endpoints](https://www.alibabacloud.com/help/zh/oss/regions-and-endpoints) for OSS Region & Endpoint parameters.
2. Start caddy
    ```console
    $ xcaddy run
    ```
3. Check that it works
    ```console
    $ open https://localhost
    ```

#### Getting started with JSON config

Create a JSON config file with the following content:
```json
{
  …
  "storage": {
    "module": "oss",
    "bucket-name": "your-bucket-name",
    "region": "your-oss-region",
    "endpoint": "your-oss-endpoint",
    "access-key-id": "your-access-key-id",
    "access-key-secret": "your-access-key-secret"
  },
  …
}
```

### Client Side Encryption

This module supports client side encryption using [google Tink](https://github.com/google/tink), thus providing a simple way to customize the encryption algorithm and handle key rotation. To get started: 

1. Install [tinkey](https://github.com/google/tink/blob/master/docs/TINKEY.md)
2. Create a key set
    ```console
    $ tinkey create-keyset --key-template AES128_GCM_RAW --out keyset.json
    ```
    Here is an example keyset.json:
    ```json
    {
      "primaryKeyId": 1818673287,
      "key": [
        {
          "keyData": {
            "typeUrl": "type.googleapis.com/google.crypto.tink.AesGcmKey",
            "value": "GhDEQ/4v72esAv3rbwZyS+ls",
            "keyMaterialType": "SYMMETRIC"
          },
          "status": "ENABLED",
          "keyId": 1818673287,
          "outputPrefixType": "RAW"
        }
      ]
    }
    ```
3. Start caddy with the following Caddyfile config
    ```
    {
      storage oss {
        bucket-name your-bucket-name
        region your-oss-region
        endpoint your-oss-endpoint
        access-key-id your-access-key-id
        access-key-secret your-access-key-secret
        encryption-key-set ./keyset.json
      }
    }
    localhost
    acme_server
    respond "Hello Caddy Storage OSS!"
    ```
4. Start caddy
    ```console
    $ xcaddy run
    $ # to rotate the key-set
    $ tinkey rotate-keyset --in keyset.json  --key-template AES128_GCM_RAW
    ```

#### Client Side Encryption with JSON config

1. Follow steps 1-2 from above to install tinkey and create a keyset.json file
2. Create a JSON config file with the following content:
    ```json
    {
      …
      "storage": {
        "module": "oss",
        "bucket-name": "your-bucket-name",
        "region": "your-oss-region",
        "endpoint": "your-oss-endpoint",
        "access-key-id": "your-access-key-id",
        "access-key-secret": "your-access-key-secret",
        "encryption-key-set": "./keyset.json"
      },
      …
    }
    ```
3. Start caddy
    ```console
    $ xcaddy run
    ```
4. To rotate the key-set
    ```console
    $ tinkey rotate-keyset --in keyset.json  --key-template AES128_GCM_RAW
    ```

### CertMagic

1. Add the package:

```console
go get github.com/aUsernameWoW/certmagic-oss
```

2. Create a `certmagicoss.NewStorage` with a `certmagicoss.Config`:

```golang
import certmagicoss "github.com/aUsernameWoW/certmagic-oss/storage"

bucket := "my-example-bucket"
region := "your-oss-region"
endpoint := "your-oss-endpoint"
accessKeyID := "your-access-key-id"
accessKeySecret := "your-access-key-secret"

oss, _ := certmagicoss.NewStorage(
  context.Background(), 
  certmagicoss.Config{
    BucketName: bucket,
    Region: region,
    Endpoint: endpoint,
    AccessKeyID: accessKeyID,
    AccessKeySecret: accessKeySecret,
  }
)
```

3. Optionally, [register as default storage](https://github.com/caddyserver/certmagic#storage).

```golang
certmagic.Default.Storage = oss
```

### Building Caddy with this module

To build Caddy with this module, you can use `xcaddy`:

1. Install xcaddy if you haven't already:
   ```console
   go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest
   ```

2. Build Caddy with the certmagic-oss module:
   ```console
   xcaddy build --with github.com/aUsernameWoW/certmagic-oss
   ```

3. If you want to specify a specific version or commit:
   ```console
   xcaddy build --with github.com/aUsernameWoW/certmagic-oss@v1.0.0
   # or
   xcaddy build --with github.com/aUsernameWoW/certmagic-oss@main
   ```

4. You can also build with other modules:
   ```console
   xcaddy build \
     --with github.com/aUsernameWoW/certmagic-oss \
     --with github.com/caddyserver/nginx-adapter
   ```

## License

This module is distributed under [Apache-2.0](LICENSE).
