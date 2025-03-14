# Enabling and Using Hardlink in Stargz Snapshotter

The `stargz-snapshotter` provides a hardlink feature to optimize storage by reducing redundancy and improving access times. This guide will walk you through enabling and using hardlinks in your setup.

## Overview

Hardlinking allows multiple references to the same file data without duplicating the data itself. This is particularly useful in environments where storage efficiency and performance are critical.

## Prerequisites

- Ensure you have `stargz-snapshotter` installed and configured.
- Familiarity with the configuration files and environment where `stargz-snapshotter` is deployed.

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

1. **Cache Initialization**: When the cache is initialized, the system checks if hardlinking is enabled and sets up the necessary components.

2. **Adding Files to Cache**: When a file is added to the cache, the system attempts to create a hardlink if it doesn't already exist.

3. **Accessing Cached Files**: When accessing a cached file, the system checks for an existing hardlink and uses it if available. This reduces the need to duplicate data and speeds up access times.

4. **Persistence and Restoration**: The state of hardlinks is periodically saved to disk. On startup, the system restores the hardlink state to ensure consistency.

5. **Periodic Cleanup**: Regular cleanup tasks remove stale or invalid hardlinks, ensuring the system remains efficient and up-to-date.

## Monitoring and Maintenance

- **Logs**: Monitor the logs for any messages related to hardlink creation, usage, and cleanup. This can help identify any issues or confirm that the feature is working as expected.
- **Performance**: Evaluate the performance improvements and storage savings achieved by enabling hardlinking.

## Troubleshooting

- **Hardlink Not Created**: Ensure that the `EnableHardlink` option is set to `true` and that the configuration file is correctly loaded by `stargz-snapshotter`.
- **File Access Issues**: Check the logs for any errors related to hardlink creation or access. Ensure that the underlying filesystem supports hardlinks.

## Conclusion

Enabling hardlinking in `stargz-snapshotter` can significantly optimize storage and improve performance. By following the steps outlined in this guide, you can leverage this feature to enhance your containerized environments.
