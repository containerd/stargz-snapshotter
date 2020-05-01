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

package sorter

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/containerd/stargz-snapshotter/cmd/ctr-remote/util"
	"github.com/containerd/stargz-snapshotter/stargz"
	"github.com/pkg/errors"
)

const (
	landmarkContents = 0xf
)

func Sort(in io.ReaderAt, log []string) (io.Reader, error) {

	// Import tar file.
	intar, err := importTar(in)
	if err != nil {
		return nil, errors.Wrap(err, "failed to sort")
	}

	// Sort the tar file in a order occurred in the given log.
	sorted := &tarFile{}
	for _, l := range log {
		moveRec(l, intar, sorted)
	}
	if len(log) == 0 {
		sorted.add(&tarEntry{
			header: &tar.Header{
				Name:     stargz.NoPrefetchLandmark,
				Typeflag: tar.TypeReg,
				Size:     int64(len([]byte{landmarkContents})),
			},
			payload: bytes.NewReader([]byte{landmarkContents}),
		})
	} else {
		sorted.add(&tarEntry{
			header: &tar.Header{
				Name:     stargz.PrefetchLandmark,
				Typeflag: tar.TypeReg,
				Size:     int64(len([]byte{landmarkContents})),
			},
			payload: bytes.NewReader([]byte{landmarkContents}),
		})
	}

	// Dump all entry and concatinate them.
	outtar := append(sorted.dump(), intar.dump()...)

	// Export tar file.
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		defer tw.Close()
		for _, entry := range outtar {
			if err := tw.WriteHeader(entry.header); err != nil {
				pw.CloseWithError(fmt.Errorf("Failed to write tar header: %v", err))
				break
			}
			if _, err := io.Copy(tw, entry.payload); err != nil {
				pw.CloseWithError(fmt.Errorf("Failed to write tar payload: %v", err))
				break
			}
		}
	}()

	return pr, nil
}

func importTar(in io.ReaderAt) (*tarFile, error) {
	tf := &tarFile{}
	pw, err := util.NewPositionWatcher(in)
	if err != nil {
		return nil, errors.Wrap(err, "failed to make position watcher")
	}
	tr := tar.NewReader(pw)

	// Walk through all nodes.
	for {
		// Fetch and parse next header.
		h, err := tr.Next()
		if err != nil {
			if err == io.EOF {
				break
			} else {
				return nil, errors.Wrap(err, "failed to parse tar file")
			}
		}
		if h.Name == stargz.PrefetchLandmark || h.Name == stargz.NoPrefetchLandmark {
			// Ignore existing landmark
			continue
		}

		// Add entry if not exist.
		if _, ok := tf.get(h.Name); ok {
			return nil, fmt.Errorf("Duplicated entry(%q) is not supported", h.Name)
		}
		tf.add(&tarEntry{
			header:  h,
			payload: io.NewSectionReader(in, pw.CurrentPos(), h.Size),
		})
	}

	return tf, nil
}

func moveRec(name string, in *tarFile, out *tarFile) {
	if name == "" {
		return
	}
	parent, _ := path.Split(strings.TrimSuffix(name, "/"))
	moveRec(parent, in, out)
	if e, ok := in.get(name); ok {
		out.add(e)
		in.remove(name)
	}
}

type tarEntry struct {
	header  *tar.Header
	payload io.ReadSeeker
}

type tarFile struct {
	index  map[string]*tarEntry
	stream []*tarEntry
}

func (f *tarFile) add(e *tarEntry) {
	if f.index == nil {
		f.index = make(map[string]*tarEntry)
	}
	f.index[e.header.Name] = e
	f.stream = append(f.stream, e)
}

func (f *tarFile) remove(name string) {
	if f.index != nil {
		delete(f.index, name)
	}
	var filtered []*tarEntry
	for _, e := range f.stream {
		if e.header.Name == name {
			continue
		}
		filtered = append(filtered, e)
	}
	f.stream = filtered
}

func (f *tarFile) get(name string) (e *tarEntry, ok bool) {
	if f.index == nil {
		return nil, false
	}
	e, ok = f.index[name]
	return
}

func (f *tarFile) dump() []*tarEntry {
	return f.stream
}
