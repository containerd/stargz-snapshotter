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

func Sort(in io.ReaderAt, log []string) ([]*TarEntry, error) {

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
		sorted.add(&TarEntry{
			Header: &tar.Header{
				Name:     stargz.NoPrefetchLandmark,
				Typeflag: tar.TypeReg,
				Size:     int64(len([]byte{landmarkContents})),
			},
			Payload: bytes.NewReader([]byte{landmarkContents}),
		})
	} else {
		sorted.add(&TarEntry{
			Header: &tar.Header{
				Name:     stargz.PrefetchLandmark,
				Typeflag: tar.TypeReg,
				Size:     int64(len([]byte{landmarkContents})),
			},
			Payload: bytes.NewReader([]byte{landmarkContents}),
		})
	}

	// Dump all entry and concatinate them.
	return append(sorted.dump(), intar.dump()...), nil
}

// ReaderFromEntries returns a reader of tar archive that contains entries passed
// through the arguments.
func ReaderFromEntries(entries ...*TarEntry) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		defer tw.Close()
		for _, entry := range entries {
			if err := tw.WriteHeader(entry.Header); err != nil {
				pw.CloseWithError(fmt.Errorf("Failed to write tar header: %v", err))
				return
			}
			if _, err := io.Copy(tw, entry.Payload); err != nil {
				pw.CloseWithError(fmt.Errorf("Failed to write tar payload: %v", err))
				return
			}
		}
		pw.Close()
	}()
	return pr
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
		tf.add(&TarEntry{
			Header:  h,
			Payload: io.NewSectionReader(in, pw.CurrentPos(), h.Size),
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

type TarEntry struct {
	Header  *tar.Header
	Payload io.ReadSeeker
}

type tarFile struct {
	index  map[string]*TarEntry
	stream []*TarEntry
}

func (f *tarFile) add(e *TarEntry) {
	if f.index == nil {
		f.index = make(map[string]*TarEntry)
	}
	f.index[e.Header.Name] = e
	f.stream = append(f.stream, e)
}

func (f *tarFile) remove(name string) {
	if f.index != nil {
		delete(f.index, name)
	}
	var filtered []*TarEntry
	for _, e := range f.stream {
		if e.Header.Name == name {
			continue
		}
		filtered = append(filtered, e)
	}
	f.stream = filtered
}

func (f *tarFile) get(name string) (e *TarEntry, ok bool) {
	if f.index == nil {
		return nil, false
	}
	e, ok = f.index[name]
	return
}

func (f *tarFile) dump() []*TarEntry {
	return f.stream
}
