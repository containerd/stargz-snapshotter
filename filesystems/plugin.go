// +build linux

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

package plugin

import (
	"context"
	"github.com/containerd/containerd/plugin"
)

// Implement each filesystem as RemoteFileSystemPlugin to mount remote layers as
// remote snapshots.
const RemoteFileSystemPlugin plugin.Type = "io.containerd.snapshotter.v1.remote"

// FileSystem is a backing filesystem abstraction.
//
// Mount() tries to mount a remote snapshot to the specified mount point
// directory. If succeed, the mountpoint directory will be treated as a layer
// snapshot.
// Check() is called to check the connectibity of the existing layer snapshot
// every time the layer is used by containerd.
type FileSystem interface {
	Mount(ctx context.Context, ref, digest, mountpoint string) error
	Check(ctx context.Context, mountpoint string) error
}
