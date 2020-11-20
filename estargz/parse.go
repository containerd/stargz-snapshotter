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

package estargz

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/google/crfs/stargz"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

// parseStargz parses the footer and TOCJSON of the stargz file
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
	dgstr := digest.Canonical.Digester()
	decodedJTOC := new(jtoc)
	if err := json.NewDecoder(io.TeeReader(tr, dgstr.Hash())).Decode(&decodedJTOC); err != nil {
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

	return &stargzBlob{
		jtoc:       decodedJTOC,
		jtocDigest: dgstr.Digest(),
		jtocOffset: tocOffset,
		payload:    io.NewSectionReader(sgz, 0, tocOffset),
		footer:     footerReader,
	}, nil
}

// combineBlobs combines passed stargz blobs and returns one reader of stargz.
func combineBlobs(sgz ...*io.SectionReader) (newSgz io.Reader, tocDgst digest.Digest, err error) {
	if len(sgz) == 0 {
		return nil, "", fmt.Errorf("at least one reader must be passed")
	}
	var blobs []*stargzBlob
	for _, r := range sgz {
		blob, err := parseStargz(r)
		if err != nil {
			return nil, "", errors.Wrapf(err, "failed to parse stargz")
		}
		blobs = append(blobs, blob)
	}
	var (
		mjtoc         = new(jtoc)
		mpayload      []io.Reader
		currentOffset int64
	)
	mjtoc.Version = blobs[0].jtoc.Version
	for _, b := range blobs {
		for _, e := range b.jtoc.Entries {
			// Recalculate Offset of non-empty files/chunks
			if (e.Type == "reg" && e.Size > 0) || e.Type == "chunk" {
				e.Offset += currentOffset
			}
			mjtoc.Entries = append(mjtoc.Entries, e)
		}
		if b.jtoc.Version < mjtoc.Version {
			mjtoc.Version = b.jtoc.Version
		}
		mpayload = append(mpayload, b.payload)
		currentOffset += b.jtocOffset
	}
	tocjson, tocDgst, err := marshalTOCJSON(mjtoc)
	if err != nil {
		return nil, "", err
	}
	footerBuf := bytes.NewBuffer(make([]byte, 0, stargz.FooterSize))
	zw, err := gzip.NewWriterLevel(footerBuf, gzip.BestCompression)
	if err != nil {
		return nil, "", err
	}
	zw.Extra = []byte(fmt.Sprintf("%016xSTARGZ", currentOffset)) // Extra header indicating the offset of TOCJSON: https://github.com/google/crfs/blob/71d77da419c90be7b05d12e59945ac7a8c94a543/stargz/stargz.go#L46
	zw.Close()
	if footerBuf.Len() != stargz.FooterSize {
		return nil, "", fmt.Errorf("failed to make the footer: invalid size %d; must be %d",
			footerBuf.Len(), stargz.FooterSize)
	}
	return io.MultiReader(
		io.MultiReader(mpayload...),
		tocjson,
		bytes.NewReader(footerBuf.Bytes()),
	), tocDgst, nil
}

// marshalTOCJSON marshals TOCJSON and returns a reader of the bytes.
func marshalTOCJSON(toc *jtoc) (io.Reader, digest.Digest, error) {
	tocJSON, err := json.Marshal(*toc)
	if err != nil {
		return nil, "", errors.Wrap(err, "failed to marshal TOC JSON")
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
	return pr, digest.FromBytes(tocJSON), nil
}
