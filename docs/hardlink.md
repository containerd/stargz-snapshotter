# Enabling and Using Hardlink in Stargz Snapshotter

The `stargz-snapshotter` provides a hardlink feature to optimize storage by reducing redundancy and improving access times. This guide will walk you through enabling and using hardlinks in your setup.

## Overview

Hardlinking allows multiple references to the same file data without duplicating the data itself. This is particularly useful in environments where storage efficiency and performance are critical. The hardlink feature in stargz-snapshotter works by:

- Tracking file chunks by their content digest
- Creating hardlinks between identical chunks across different container layers

## Prerequisites

- Ensure you have `stargz-snapshotter` installed and configured.
- Familiarity with the configuration files and environment where `stargz-snapshotter` is deployed.
- A filesystem that supports hardlinks (most Linux filesystems do).

## Enabling Hardlinking

To enable hardlinking, you need to modify the configuration file of `stargz-snapshotter`.

1. **Locate the Configuration File**: The configuration file is typically named `config.toml` or similar, depending on your setup.

2. **Modify the Configuration**:
   - Open the configuration file in a text editor.
   - Locate the `DirectoryCacheConfig` section.
   - Set the `EnableHardlink` option to `true`.

   Example:
   ```toml
   [directory_cache]
   enable_hardlink = true
   ```

3. **Initialize the Hardlink Manager in main**:
   In your entrypoint (e.g., `cmd/containerd-stargz-grpc/main.go`), initialize and register the global hardlink manager with an appropriate root directory (it will create a `hardlinks/` subdirectory under this root):

   ```go
   import "github.com/containerd/stargz-snapshotter/hardlink"

   hlm, err := hardlink.NewHardlinkManager(rootDir)
   hardlink.SetGlobalManager(hlm)
   ```

   The cache layer will automatically use this global manager when `enable_hardlink = true`.

## Using Hardlinking

Once hardlinking is enabled and the manager is initialized, `stargz-snapshotter` will automatically manage hardlinks for cached files. Here's how it works:

1. **Cache Initialization**: When the cache is initialized, the system checks if hardlinking is enabled and uses the global hardlink manager. It will:
   - Create a hardlink directory structure under the configured root
   - Use in-memory LRU-backed mappings (no on-disk persistence)

2. **Adding Files to Cache**: When a file is added to the cache:
   - The system calculates and uses the file's content digest
   - It checks if a file with the same digest already exists
   - If found, it creates a hardlink rather than duplicating the content
   - It maps the cache key to the digest for future lookups

3. **Accessing Cached Files**: When accessing a cached file:
   - The system first checks for the cache key/digest in memory
   - It then retrieves the hardlinked file path if available
   - If the underlying file is missing, the mapping is removed on-the-fly and the operation falls back to creating a new file

## Technical Details

The hardlink feature works through several components:

- **HardlinkManager**: Centralized service that manages digest tracking and hardlink creation
- **LRU-backed Digest Mapping**: In-memory LRU cache mapping digest → file path
- **Reverse Indexes**: Plain maps for key → digest and digest → keys, pruned on eviction

When prefetching or downloading content, the chunk digest is passed to the cache system via the `ChunkDigest` option, which enables the hardlink manager to track and link identical content.
