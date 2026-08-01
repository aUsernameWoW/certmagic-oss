# Changelog

## [Unreleased]

### Added
- Initial implementation of CertMagic storage backend for Alibaba Cloud OSS
- Support for basic storage operations (Store, Load, Delete, List, Stat)
- Support for distributed locking mechanism using OSS object markers
- Client-side encryption support using Google Tink
- Caddy module integration

### Changed
- N/A

### Deprecated
- N/A

### Removed
- N/A

### Fixed
- Non-recursive `List` never returned "subdirectories": with a delimiter set, OSS reports them as `CommonPrefixes`, which were ignored, so listing a prefix containing only directories (e.g. Caddy's `ech/configs`) always came back empty. This made Caddy generate and store a brand-new ECH config on every provision instead of reusing existing ones. `List` now returns both objects and common prefixes (without trailing slash), and normalizes the prefix with a trailing `/` so sibling keys sharing the string prefix are excluded — matching certmagic `FileStorage` semantics.

### Security
- Added client-side encryption option for securing certificates at rest