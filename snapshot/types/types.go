/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package types

import "context"

// FileSystem is a backing filesystem abstraction.
type FileSystem interface {
	// Mount tries to mount a remote snapshot to the specified mount point
	// directory. If succeed, the mountpoint directory will be treated as a layer
	// snapshot. If Mount fails, the mountpoint directory MUST be cleaned up.
	Mount(ctx context.Context, mountpoint string, labels map[string]string) error

	// Check is called to check the connectibity of the existing layer snapshot
	// every time the layer is used by containerd.
	Check(ctx context.Context, mountpoint string, labels map[string]string) error

	// Unmount is called to unmount a remote snapshot from the specified mount point
	// directory.
	Unmount(ctx context.Context, mountpoint string) error
}
