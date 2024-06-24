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

package recorder

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/errdefs"
	"github.com/containerd/stargz-snapshotter/recorder"
	"github.com/containerd/stargz-snapshotter/util/testutil"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/rs/xid"
)

var allowedPrefix = [4]string{"", "./", "/", "../"}

func TestNodeIndex(t *testing.T) {
	type recordEntry struct {
		name  string
		index int
	}
	tests := []struct {
		name     string
		in       [][]testutil.TarEntry
		record   []string
		want     []recordEntry
		wantFail bool
	}{
		{
			name: "single layer",
			in: [][]testutil.TarEntry{
				{
					testutil.File("foo", "foo"),
					testutil.File("bar", "bar"),
				},
			},
			record: []string{"bar", "foo"},
			want: []recordEntry{
				{"bar", 0},
				{"foo", 0},
			},
		},
		{
			name: "overlay",
			in: [][]testutil.TarEntry{
				{
					testutil.File("foo", "foo"),
					testutil.Dir("bar/"),
					testutil.Dir("bar/barbar/"),
					testutil.File("bar/barbar/bar2", "bar2"),
					testutil.File("bar/barbar/foo2", "foo2"),
					testutil.File("x", "x"),
					testutil.File("y", "y"),
				},
				{
					testutil.File("foo", "foo2"),
					testutil.File("baz", "baz"),
					testutil.Dir("bar/"),
					testutil.Dir("bar/barbar/"),
					testutil.File("bar/barbar/foo2", "foo2-upper"),
				},
				{
					testutil.File("foo", "foo3"),
					testutil.Dir("a/"),
					testutil.File("a/aaaaaa", "a"),
					testutil.File("y", "y"),
				},
			},
			record: []string{
				"baz",
				"foo",
				"bar/barbar/foo2",
				"a/aaaaaa",
				"bar/barbar/bar2",
				"y"},
			want: []recordEntry{
				{"baz", 1},
				{"foo", 2},
				{"bar/barbar/foo2", 1},
				{"a/aaaaaa", 2},
				{"bar/barbar/bar2", 0},
				{"y", 2},
			},
		},
		{
			name: "various files",
			in: [][]testutil.TarEntry{
				// All file path should be interpreted as the path relative to root.
				{
					testutil.File("foo", "foo"),
					testutil.Dir("bar/"),
					testutil.Symlink("bar/foosym", "foo"),
					testutil.Dir("bar/barbar/"),
					testutil.File("./bar/barbar/bar2", "bar2"),
					testutil.File("../bar/barbar/foo2", "foo2"),
					testutil.File("/x", "x"),
					testutil.Chardev("chardev", 1, 10),
					testutil.Fifo("bar/fifo1"),
				},
				{
					testutil.File("./foo", "foo2"),
					testutil.File("baz", "baz"),
					testutil.Dir("bar/"),
					testutil.Dir("./bar/barbar/"),
					testutil.File("bar/barbar/bazlink", "baz"),
					testutil.File("../bar/barbar/foo2", "foo2-upper"),
					testutil.Chardev("chardev", 10, 100),
					testutil.Chardev("./blockdev", 100, 1),
				},
			},
			record: []string{
				// All file path should be interpreted as the path relative to root.
				"./bar/foosym",
				"bar/barbar/bazlink",
				"/bar/barbar/foo2",
				"chardev",
				"../blockdev",
				"bar/fifo1",
				"./bar/barbar/bar2",
			},
			want: []recordEntry{
				{"bar/foosym", 0},
				{"bar/barbar/bazlink", 1},
				{"bar/barbar/foo2", 1},
				{"chardev", 1},
				{"blockdev", 1},
				{"bar/fifo1", 0},
				{"bar/barbar/bar2", 0},
			},
		},
		{
			name: "whiteout file",
			in: [][]testutil.TarEntry{
				{
					testutil.Dir("bar/"),
					testutil.File("bar/barfile", "bar"),
				},
				{
					testutil.Dir("bar/"),
					testutil.File(path.Join("bar", whiteoutPrefix+"barfile"), "bar"),
				},
			},
			record:   []string{"bar/barfile"},
			want:     []recordEntry{},
			wantFail: true,
		},
		{
			name: "whiteout dir",
			in: [][]testutil.TarEntry{
				{
					testutil.Dir("bar/"),
					testutil.File("bar/barfile", "bar"),
				},
				{
					testutil.Dir("bar/"),
					testutil.Dir(path.Join("bar", whiteoutOpaqueDir) + "/"),
				},
			},
			record:   []string{"bar/barfile"},
			want:     []recordEntry{},
			wantFail: true,
		},
		{
			name: "whiteout dir",
			in: [][]testutil.TarEntry{
				{
					testutil.Dir("bar/"),
					testutil.File("bar/barfile", "bar"),
				},
				{
					testutil.Dir("bar/"),
					testutil.Dir(path.Join("bar", whiteoutOpaqueDir) + "/"),
				},
			},
			record:   []string{"bar/barfile"},
			want:     []recordEntry{},
			wantFail: true,
		},
	}

	tempDir, err := os.MkdirTemp("", "test-recorder")
	if err != nil {
		t.Fatalf("failed to prepare content store dir: %v", err)
	}
	defer os.RemoveAll(tempDir)
	cs, err := local.NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to prepare content store: %v", err)
	}
	compressWrappers := map[string]func(r io.Reader) io.Reader{
		ocispec.MediaTypeImageLayer:     func(r io.Reader) io.Reader { return r }, // nop (uncompressed)
		ocispec.MediaTypeImageLayerGzip: gzipCompress,                             // gzip compression
	}
	ctx := context.Background()
	for _, tt := range tests {
		for _, prefix := range allowedPrefix {
			prefix := prefix
			for mediatype, cWrapper := range compressWrappers {
				t.Run(tt.name+":"+mediatype+",prefix="+prefix, func(t *testing.T) {
					var layers []ocispec.Descriptor
					for _, es := range tt.in {
						ref := fmt.Sprintf("recorder-test-%v", xid.New().String())
						lw, err := content.OpenWriter(ctx, cs, content.WithRef(ref))
						if err != nil {
							t.Errorf("failed to open writer: %v", err)
							return
						}
						tarR := testutil.BuildTar(es, testutil.WithPrefix(prefix))
						if _, err := io.Copy(lw, cWrapper(tarR)); err != nil {
							t.Errorf("failed to copy layer: %v", err)
							return
						}
						if err := lw.Commit(ctx, 0, ""); err != nil && !errdefs.IsAlreadyExists(err) {
							t.Errorf("failed to commit layer: %v", err)
							return
						}
						info, err := cs.Info(ctx, lw.Digest())
						if err != nil {
							t.Errorf("failed to get layer info: %v", err)
							return
						}
						// TODO: check compressed version as well
						layers = append(layers, ocispec.Descriptor{
							Digest:    info.Digest,
							Size:      info.Size,
							MediaType: mediatype,
						})
					}
					ir, err := imageRecorderFromManifest(ctx, cs,
						ocispec.Descriptor{}, ocispec.Manifest{Layers: layers})
					if err != nil {
						t.Errorf("failed to get recorder: %v", err)
						return
					}
					defer ir.Close()
					fail := false
					for _, name := range tt.record {
						if err := ir.Record(name); err != nil {
							fail = true
							t.Logf("failed to record: %q: %v", name, err)
							break
						}
					}
					if tt.wantFail != fail {
						t.Errorf("unexpected record result: fail = %v; wantFail = %v", fail, tt.wantFail)
						return
					}
					if tt.wantFail {
						return // no need to check the record out
					}
					recordOut, err := ir.Commit(ctx)
					if err != nil {
						t.Errorf("failed to commit record: %v", err)
						return
					}

					ra, err := cs.ReaderAt(ctx, ocispec.Descriptor{Digest: recordOut})
					if err != nil {
						t.Errorf("failed to get record out: %v", err)
						return
					}
					defer ra.Close()
					dec := json.NewDecoder(io.NewSectionReader(ra, 0, ra.Size()))
					i := 0
					for dec.More() {
						var e recorder.Entry
						if err := dec.Decode(&e); err != nil {
							t.Errorf("failed to decord record: %v", err)
							return
						}
						if len(tt.want) <= i {
							t.Errorf("too many records: %d th but want %d", i, len(tt.want))
							return
						}
						if e.Path != tt.want[i].name || *e.LayerIndex != tt.want[i].index {
							t.Errorf("unexpected entry { name = %q, index = %d }; want { name = %q, index = %d }", e.Path, *e.LayerIndex, tt.want[i].name, tt.want[i].index)
							return
						}
						i++
					}
					if i < len(tt.want) {
						t.Errorf("record is too short: %d; but want %d", i, len(tt.want))
						return
					}
				})
			}
		}
	}
}

func gzipCompress(r io.Reader) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		gw := gzip.NewWriter(pw)
		if _, err := io.Copy(gw, r); err != nil {
			gw.Close()
			pw.CloseWithError(err)
			return
		}
		gw.Close()
		pw.Close()
	}()
	return pr
}
