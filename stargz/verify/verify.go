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

package verify

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/google/crfs/stargz"
	"github.com/pkg/errors"
)

const (
	// TOCJSONDigestAnnotation is an annotation for image manifest. This stores the
	// digest of the TOC JSON
	TOCJSONDigestAnnotation = "containerd.io/snapshot/stargz/toc.digest"
)

// jtoc is a TOC JSON schema which contains additional fields for chunk-level
// content verification. This is still backward compatible to the official
// definition:
// https://github.com/google/crfs/blob/71d77da419c90be7b05d12e59945ac7a8c94a543/stargz/stargz.go
type jtoc struct {

	// Version is a field to store the version information of TOC JSON. This field
	// is unused in this package but is for the backwards-compatibility to stargz.
	Version int `json:"version"`

	// Entries is a field to store TOCEntries in this archive. These TOCEntries
	// are extended in this package for content verification, with keeping the
	// backward-compatibility with stargz.
	Entries []*TOCEntry `json:"entries"`
}

// TOCEntry extends stargz.TOCEntry with an additional field for storing chunks
// digests.
type TOCEntry struct {
	stargz.TOCEntry

	// ChunkDigest stores a digest of the chunk. This must be formed
	// as "sha256:0123abcd...".
	ChunkDigest string `json:"chunkDigest,omitempty"`
}

// NewVerifiableStagz takes stargz archive and returns modified one which contains
// contents digests in the TOC JSON. This also returns the digest of TOC JSON
// itself which can be used for verifying the TOC JSON.
func NewVerifiableStagz(sgz *io.SectionReader) (newSgz io.Reader, jtocDigest string, err error) {
	// Extract TOC JSON and some related parts
	toc, tocOffset, footer, err := extractTOCJSON(sgz)
	if err != nil {
		return nil, "", errors.Wrap(err, "failed to extract TOC JSON")
	}

	// Rewrite the original TOC JSON with chunks digests
	if err := addDigests(toc, sgz); err != nil {
		return nil, "", errors.Wrap(err, "failed to digest stargz")
	}
	tocJSON, err := json.Marshal(toc)
	if err != nil {
		return nil, "", errors.Wrap(err, "failed to marshal TOC JSON")
	}
	tocHash := sha256.New()
	if _, err := io.CopyN(tocHash, bytes.NewReader(tocJSON), int64(len(tocJSON))); err != nil {
		return nil, "", errors.Wrap(err, "failed to calculate digest of TOC JSON")
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

	// Reconstruct stargz file with the modified TOC JSON
	if _, err := sgz.Seek(0, io.SeekStart); err != nil {
		return nil, "", errors.Wrap(err, "failed to reset the seek position of stargz")
	}
	if _, err := footer.Seek(0, io.SeekStart); err != nil {
		return nil, "", errors.Wrap(err, "failed to reset the seek position of footer")
	}
	return io.MultiReader(
		io.LimitReader(sgz, tocOffset), // Original stargz (before TOC JSON)
		pr,                             // Rewritten TOC JSON
		footer,                         // Unmodified footer (because tocOffset is unchanged)
	), fmt.Sprintf("sha256:%x", tocHash.Sum(nil)), nil
}

// extractTOCJSON extracts TOC JSON from the given stargz reader, with avoiding
// scanning the entire blob leveraging its footer. The parsed TOC JSON contains
// additional fields usable for chunk-level content verification.
func extractTOCJSON(sgz *io.SectionReader) (toc *jtoc, tocOffset int64, footer io.ReadSeeker, err error) {
	// Parse stargz footer and get the offset of TOC JSON
	if sgz.Size() < stargz.FooterSize {
		return nil, 0, nil, errors.New("stargz data is too small")
	}
	footerReader := io.NewSectionReader(sgz, sgz.Size()-stargz.FooterSize, stargz.FooterSize)
	zr, err := gzip.NewReader(footerReader)
	if err != nil {
		return nil, 0, nil, errors.Wrap(err, "failed to uncompress footer")
	}
	defer zr.Close()
	if len(zr.Header.Extra) != 22 {
		return nil, 0, nil, errors.Wrap(err, "invalid extra size; must be 22 bytes")
	} else if string(zr.Header.Extra[16:]) != "STARGZ" {
		return nil, 0, nil, errors.New("invalid footer; extra must contain magic string \"STARGZ\"")
	}
	tocOffset, err = strconv.ParseInt(string(zr.Header.Extra[:16]), 16, 64)
	if err != nil {
		return nil, 0, nil, errors.Wrap(err, "invalid footer; failed to get the offset of TOC JSON")
	} else if tocOffset > sgz.Size() {
		return nil, 0, nil, fmt.Errorf("invalid footer; offset of TOC JSON is too large (%d > %d)",
			tocOffset, sgz.Size())
	}

	// Decode the TOC JSON
	tocReader := io.NewSectionReader(sgz, tocOffset, sgz.Size()-tocOffset-stargz.FooterSize)
	if err := zr.Reset(tocReader); err != nil {
		return nil, 0, nil, errors.Wrap(err, "failed to uncompress TOC JSON targz entry")
	}
	tr := tar.NewReader(zr)
	h, err := tr.Next()
	if err != nil {
		return nil, 0, nil, errors.Wrap(err, "failed to get TOC JSON tar entry")
	} else if h.Name != stargz.TOCTarName {
		return nil, 0, nil, fmt.Errorf("invalid TOC JSON tar entry name %q; must be %q",
			h.Name, stargz.TOCTarName)
	}
	toc = new(jtoc)
	if err := json.NewDecoder(tr).Decode(&toc); err != nil {
		return nil, 0, nil, errors.Wrap(err, "failed to decode TOC JSON")
	}
	if _, err := tr.Next(); err != io.EOF {
		// We only accept stargz file that its TOC JSON resides at the end of that
		// file to avoid changing the offsets of the following file entries by
		// rewriting TOC JSON (The official stargz lib also puts TOC JSON at the end
		// of the stargz file at this mement).
		// TODO: in the future, we should relax this restriction.
		return nil, 0, nil, errors.New("TOC JSON must reside at the end of targz")
	}

	return toc, tocOffset, footerReader, nil
}

// addDigests calculates chunk digests and adds them to the TOC JSON
func addDigests(toc *jtoc, sgz *io.SectionReader) error {
	var (
		sizeMap = make(map[string]int64)
		zr      *gzip.Reader
		err     error
	)
	for _, e := range toc.Entries {
		if e.Type == "reg" || e.Type == "chunk" {
			if e.Type == "reg" {
				sizeMap[e.Name] = e.Size // saves the file size for futural use
			}
			size := e.ChunkSize
			if size == 0 {
				// In the seriarized form, ChunkSize field of the last chunk in a file
				// is zero so we need to calculate it.
				size = sizeMap[e.Name] - e.ChunkOffset
			}
			if size > 0 {
				if _, err := sgz.Seek(e.Offset, io.SeekStart); err != nil {
					return errors.Wrapf(err, "failed to seek to %q", e.Name)
				}
				if zr == nil {
					if zr, err = gzip.NewReader(sgz); err != nil {
						return errors.Wrapf(err, "failed to decomp %q", e.Name)
					}
				} else {
					if err := zr.Reset(sgz); err != nil {
						return errors.Wrapf(err, "failed to decomp %q", e.Name)
					}
				}
				digest := sha256.New()
				if _, err := io.CopyN(digest, zr, size); err != nil {
					return errors.Wrapf(err, "failed to read %q; (file size: %d, e.Offset: %d, e.ChunkOffset: %d, chunk size: %d, end offset: %d, size of stargz file: %d)",
						e.Name, sizeMap[e.Name], e.Offset, e.ChunkOffset, size,
						e.Offset+size, sgz.Size())
				}
				e.ChunkDigest = fmt.Sprintf("sha256:%x", digest.Sum(nil))
			}
		}
	}

	return nil
}
