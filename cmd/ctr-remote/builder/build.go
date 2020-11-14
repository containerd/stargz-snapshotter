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

package builder

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"runtime"
	"strconv"

	"github.com/containerd/containerd/log"
	"github.com/containerd/stargz-snapshotter/cmd/ctr-remote/sorter"
	"github.com/google/crfs/stargz"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

// jtoc is the TOC JSON schema of stargz
// https://github.com/google/crfs/blob/71d77da419c90be7b05d12e59945ac7a8c94a543/stargz/stargz.go
type jtoc struct {

	// Version is a field to store the version information of TOC JSON.
	Version int `json:"version"`

	// Entries is a field to store TOCEntries in this archive.
	Entries []*stargz.TOCEntry `json:"entries"`
}

type options struct {
	chunkSize int
}

type Option func(o *options)

func WithChunkSize(chunkSize int) Option {
	return func(o *options) {
		o.chunkSize = chunkSize
	}
}

// BuildStargz builds a stargz layer according to the passed entries and write the result
// to the specified writer. This function builds a stargz blob in parallel, with dividing
// that blob into several (at least the number of runtime.GOMAXPROCS(0)) sub-blobs.
func BuildStargz(ctx context.Context, entries []*sorter.TarEntry, w io.Writer, opt ...Option) error {
	var opts options
	for _, o := range opt {
		o(&opts)
	}
	var sgzParts []*os.File
	var eg errgroup.Group
	for i, parts := range divideEntries(entries, runtime.GOMAXPROCS(0)) {
		partFile, err := ioutil.TempFile("", "partdata")
		if err != nil {
			return err
		}
		defer func() {
			if err := partFile.Close(); err != nil {
				log.G(ctx).WithError(err).Warnf("failed to close tmpfile %v",
					partFile.Name())
			}
			if err := os.Remove(partFile.Name()); err != nil {
				log.G(ctx).WithError(err).Warnf("failed to remove tmpfile %v",
					partFile.Name())
			}
		}()
		sgzParts = append(sgzParts, partFile)
		i, parts := i, parts
		eg.Go(func() error {
			log.G(ctx).WithField("entries", len(parts)).Debugf("converting(%d)", i)
			defer log.G(ctx).WithField("entries", len(parts)).Debugf("converted(%d)", i)
			sw := stargz.NewWriter(partFile)
			sw.ChunkSize = opts.chunkSize
			if err := sw.AppendTar(sorter.ReaderFromEntries(parts...)); err != nil {
				return err
			}
			return sw.Close()
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}
	var partBlobs []*io.SectionReader
	for _, f := range sgzParts {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		partBlobs = append(partBlobs, io.NewSectionReader(f, 0, info.Size()))
	}
	whole, err := combineBlobs(partBlobs...)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, whole)
	return err
}

// divideEntries divides passed entries to the parts at least the number specified by the
// argument.
func divideEntries(entries []*sorter.TarEntry, minPartsNum int) (set [][]*sorter.TarEntry) {
	var estimatedSize int64
	for _, e := range entries {
		estimatedSize += e.Header.Size
	}
	unitSize := estimatedSize / int64(minPartsNum)
	var (
		nextEnd = unitSize
		offset  int64
	)
	set = append(set, []*sorter.TarEntry{})
	for _, e := range entries {
		set[len(set)-1] = append(set[len(set)-1], e)
		offset += e.Header.Size
		if offset > nextEnd {
			set = append(set, []*sorter.TarEntry{})
			nextEnd += unitSize
		}
	}
	return
}

type stargzBlob struct {
	payload     io.Reader
	payloadSize int64
	tocJSON     *jtoc
}

// combineBlobs combines passed stargz blobs and returns one reader of stargz.
func combineBlobs(sgz ...*io.SectionReader) (newSgz io.Reader, err error) {
	if len(sgz) == 0 {
		return nil, fmt.Errorf("at least one reader must be passed")
	}
	var blobs []*stargzBlob
	for _, r := range sgz {
		blob, err := parseStargz(r)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to parse stargz")
		}
		blobs = append(blobs, blob)
	}
	var (
		mjtoc         = new(jtoc)
		mpayload      []io.Reader
		currentOffset int64
	)
	mjtoc.Version = blobs[0].tocJSON.Version
	for _, b := range blobs {
		for _, e := range b.tocJSON.Entries {
			// Recalculate Offset of non-empty files/chunks
			if (e.Type == "reg" && e.Size > 0) || e.Type == "chunk" {
				e.Offset += currentOffset
			}
			mjtoc.Entries = append(mjtoc.Entries, e)
		}
		if b.tocJSON.Version < mjtoc.Version {
			mjtoc.Version = b.tocJSON.Version
		}
		mpayload = append(mpayload, b.payload)
		currentOffset += b.payloadSize
	}
	tocjson, err := marshalTOCJSON(mjtoc)
	if err != nil {
		return nil, err
	}
	footerBuf := bytes.NewBuffer(make([]byte, 0, stargz.FooterSize))
	zw, err := gzip.NewWriterLevel(footerBuf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	zw.Extra = []byte(fmt.Sprintf("%016xSTARGZ", currentOffset)) // Extra header indicating the offset of TOCJSON: https://github.com/google/crfs/blob/71d77da419c90be7b05d12e59945ac7a8c94a543/stargz/stargz.go#L46
	zw.Close()
	if footerBuf.Len() != stargz.FooterSize {
		return nil, fmt.Errorf("failed to make the footer: invalid size %d; must be %d",
			footerBuf.Len(), stargz.FooterSize)
	}
	return io.MultiReader(
		io.MultiReader(mpayload...),
		tocjson,
		bytes.NewReader(footerBuf.Bytes()),
	), nil
}

// marshalTOCJSON marshals TOCJSON and returns a reader of the bytes.
func marshalTOCJSON(toc *jtoc) (io.Reader, error) {
	tocJSON, err := json.Marshal(*toc)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal TOC JSON")
	}
	pr, pw := io.Pipe()
	go func() {
		zw, err := gzip.NewWriterLevel(pw, gzip.BestCompression)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		zw.Extra = []byte("stargz.toc") // this magic string might not be necessary but let's follow the official behaviour: https://github.com/google/crfs/blob/71d77da419c90be7b05d12e59945ac7a8c94a543/stargz/stargz.go#L596
		tw := tar.NewWriter(zw)
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     stargz.TOCTarName,
			Size:     int64(len(tocJSON)),
		}); err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := tw.Write(tocJSON); err != nil {
			pw.CloseWithError(err)
			return
		}
		if err := tw.Close(); err != nil {
			pw.CloseWithError(err)
			return
		}
		if err := zw.Close(); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.Close()
	}()
	return pr, nil
}

// parseStargz parses the footer and TOCJSON of the stargz file
// TODO: We have logics similar to this func in several places in this project so
//       we should separate these logics out into a new pkg.
func parseStargz(sgz *io.SectionReader) (blob *stargzBlob, err error) {
	// Parse stargz footer and get the offset of TOC JSON
	if sgz.Size() < stargz.FooterSize {
		return nil, errors.New("stargz data is too small")
	}
	footerReader := io.NewSectionReader(sgz, sgz.Size()-stargz.FooterSize, stargz.FooterSize)
	zr, err := gzip.NewReader(footerReader)
	if err != nil {
		return nil, errors.Wrap(err, "failed to uncompress footer")
	}
	defer zr.Close()
	if len(zr.Header.Extra) != 22 {
		return nil, errors.Wrap(err, "invalid extra size; must be 22 bytes")
	} else if string(zr.Header.Extra[16:]) != "STARGZ" {
		return nil, errors.New("invalid footer; extra must contain magic string \"STARGZ\"")
	}
	tocOffset, err := strconv.ParseInt(string(zr.Header.Extra[:16]), 16, 64)
	if err != nil {
		return nil, errors.Wrap(err, "invalid footer; failed to get the offset of TOC JSON")
	} else if tocOffset > sgz.Size() {
		return nil, fmt.Errorf("invalid footer; offset of TOC JSON is too large (%d > %d)",
			tocOffset, sgz.Size())
	}

	// Decode the TOC JSON
	tocReader := io.NewSectionReader(sgz, tocOffset, sgz.Size()-tocOffset-stargz.FooterSize)
	if err := zr.Reset(tocReader); err != nil {
		return nil, errors.Wrap(err, "failed to uncompress TOC JSON targz entry")
	}
	tr := tar.NewReader(zr)
	h, err := tr.Next()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get TOC JSON tar entry")
	} else if h.Name != stargz.TOCTarName {
		return nil, fmt.Errorf("invalid TOC JSON tar entry name %q; must be %q",
			h.Name, stargz.TOCTarName)
	}
	decodedJTOC := new(jtoc)
	if err := json.NewDecoder(tr).Decode(&decodedJTOC); err != nil {
		return nil, errors.Wrap(err, "failed to decode TOC JSON")
	}
	if _, err := tr.Next(); err != io.EOF {
		// We only accept stargz file that its TOC JSON resides at the end of that
		// file to avoid changing the offsets of the following file entries by
		// rewriting TOC JSON (The official stargz lib also puts TOC JSON at the end
		// of the stargz file at this mement).
		// TODO: in the future, we should relax this restriction.
		return nil, errors.New("TOC JSON must reside at the end of targz")
	}

	return &stargzBlob{io.LimitReader(sgz, tocOffset), tocOffset, decodedJTOC}, nil
}
