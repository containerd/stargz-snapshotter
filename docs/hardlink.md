# Enabling and Using Hardlink in Stargz Snapshotter

The `stargz-snapshotter` provides a hardlink feature to optimize storage by reducing redundancy and improving access times. This guide will walk you through enabling and using hardlinks in your setup.

## Overview

Hardlinking allows multiple references to the same file data without duplicating the data itself. This is particularly useful in environments where storage efficiency and performance are critical. The hardlink feature in stargz-snapshotter works by:

- Tracking file chunks by their content digest
- Creating hardlinks between identical chunks across different container layers
- Maintaining a persistent mapping between content digests and file paths
- Automatically cleaning up unused hardlinks

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

3. **Save and Close the File**: After making the changes, save the file and close the editor.

## Using Hardlinking

Once hardlinking is enabled, `stargz-snapshotter` will automatically manage hardlinks for cached files. Here's how it works:

1. **Cache Initialization**: When the cache is initialized, the system checks if hardlinking is enabled and sets up the necessary components, including:
   - Creating a hardlink directory structure
   - Loading existing hardlink mappings from the links file
   - Starting background tasks for cleanup and persistence

2. **Adding Files to Cache**: When a file is added to the cache:
   - The system calculates and stores the file's content digest
   - It checks if a file with the same digest already exists
   - If found, it creates a hardlink rather than duplicating the content
   - It maps the cache key to the digest for future lookups

3. **Accessing Cached Files**: When accessing a cached file:
   - The system first checks for the cache key in memory
   - If not found, it looks up the digest mapping
   - It then retrieves the hardlinked file path
   - This reduces disk I/O and improves performance

4. **Persistence and Restoration**: The hardlink manager:
   - Periodically persists digest-to-file and key-to-digest mappings to disk (every 5 seconds)
   - Restores these mappings on startup
   - Validates that all mapped files still exist during restoration

5. **Periodic Cleanup**: The system runs a cleanup process every 24 hours that:
   - Removes unused digest mappings
   - Validates file existence
   - Maintains the mapping database

## Technical Details

The hardlink feature works through several components:

- **HardlinkManager**: Centralized service that manages digest tracking and hardlink creation
- **Digest Mapping**: Tracks which files have the same content
- **Key-to-Digest Mapping**: Associates cache keys with their content digests
- **Links File**: JSON file that stores all mappings for persistence
- **Background Workers**: Handle persistence and cleanup tasks

When prefetching or downloading content, the chunk digest is passed to the cache system via the `ChunkDigest` option, which enables the hardlink manager to track and link identical content.
