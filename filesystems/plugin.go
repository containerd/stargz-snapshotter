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
	"github.com/containerd/containerd/plugin"
)

// We use RemoteFileSystemPlugin to mount unpacked remote layers as remote
// snapshots.
const RemoteFileSystemPlugin plugin.Type = "io.containerd.snapshotter.v1.remote"

// FileSystem is a backing filesystem abstraction.
//
// On each Prepare()-ing a snapshot, we get a new Mounter for each FileSystem
// and try to Mounter.Prepare() with the basic information of the layer(ref,
// layer digest). If the Prepare()-ing succeeded for the layer on a FileSystem,
// we treat the layer as being backed by the filesystem and being a remote
// snapshot.
//
// On each Commit(), this snapshotter checks if the active snapshot is a remote
// snapshot. If so, this snapshotter invokes the corresponding Mounter.Mount()
// to mount the unpacked remote layer into the committed snapshot directory.
type FileSystem interface {
	Mounter() Mounter
}

// Mounter prepares and mounts unpacked remote layers to the specified snapshot
// directories.
//
// Prepare() is used to attempt to prepare a layer as a remote snapshot with the
// layer's basic information(ref and the layer digest). Mounter can check if
// this layer can be mounted as a remote snapshot. If possible, does any
// necessary initialization.
//
// Mount() is used to mount the prepared layer on the committed snapshot
// directory as a remote snapshot.
type Mounter interface {
	Prepare(ref, digest string) error
	Mount(target string) error
}
