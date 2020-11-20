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
	"strings"
	"testing"

	"github.com/google/crfs/stargz"
	digest "github.com/opencontainers/go-digest"
)

const chunkSize = 3

type check func(t *testing.T, sgzData []byte, tocDigest digest.Digest, dgstMap map[string]digest.Digest)

// TestDigestAndVerify runs specified checks against sample stargz blobs.
func TestDigestAndVerify(t *testing.T) {
	tests := []struct {
		name    string
		tarInit func(t *testing.T, dgstMap map[string]digest.Digest) (blob *io.SectionReader)
		checks  []check
	}{
		{
			name: "no-regfile",
			tarInit: func(t *testing.T, dgstMap map[string]digest.Digest) (blob *io.SectionReader) {
				return tarBlob(t,
					directory("test/"),
				)
			},
			checks: []check{
				checkStargzTOC,
				checkVerifyTOC,
				checkVerifyInvalidStargzFail(tarBlob(t,
					directory("test2/"), // modified
				)),
			},
		},
		{
			name: "small-files",
			tarInit: func(t *testing.T, dgstMap map[string]digest.Digest) (blob *io.SectionReader) {
				return tarBlob(t,
					regDigest(t, "baz.txt", "", dgstMap),
					regDigest(t, "foo.txt", "a", dgstMap),
					directory("test/"),
					regDigest(t, "test/bar.txt", "bbb", dgstMap),
				)
			},
			checks: []check{
				checkStargzTOC,
				checkVerifyTOC,
				checkVerifyInvalidStargzFail(tarBlob(t,
					reg("baz.txt", ""),
					reg("foo.txt", "M"), // modified
					directory("test/"),
					reg("test/bar.txt", "bbb"),
				)),
				checkVerifyInvalidTOCEntryFail("foo.txt"),
				checkVerifyBrokenContentFail("foo.txt"),
			},
		},
		{
			name: "big-files",
			tarInit: func(t *testing.T, dgstMap map[string]digest.Digest) (blob *io.SectionReader) {
				return tarBlob(t,
					regDigest(t, "baz.txt", "bazbazbazbazbazbazbaz", dgstMap),
					regDigest(t, "foo.txt", "a", dgstMap),
					directory("test/"),
					regDigest(t, "test/bar.txt", "testbartestbar", dgstMap),
				)
			},
			checks: []check{
				checkStargzTOC,
				checkVerifyTOC,
				checkVerifyInvalidStargzFail(tarBlob(t,
					reg("baz.txt", "bazbazbazMMMbazbazbaz"), // modified
					reg("foo.txt", "a"),
					directory("test/"),
					reg("test/bar.txt", "testbartestbar"),
				)),
				checkVerifyInvalidTOCEntryFail("test/bar.txt"),
				checkVerifyBrokenContentFail("test/bar.txt"),
			},
		},
		{
			name: "with-non-regfiles",
			tarInit: func(t *testing.T, dgstMap map[string]digest.Digest) (blob *io.SectionReader) {
				return tarBlob(t,
					regDigest(t, "baz.txt", "bazbazbazbazbazbazbaz", dgstMap),
					regDigest(t, "foo.txt", "a", dgstMap),
					symlink("barlink", "test/bar.txt"),
					directory("test/"),
					regDigest(t, "test/bar.txt", "testbartestbar", dgstMap),
					directory("test2/"),
					link("test2/bazlink", "baz.txt"),
				)
			},
			checks: []check{
				checkStargzTOC,
				checkVerifyTOC,
				checkVerifyInvalidStargzFail(tarBlob(t,
					reg("baz.txt", "bazbazbazbazbazbazbaz"),
					reg("foo.txt", "a"),
					symlink("barlink", "test/bar.txt"),
					directory("test/"),
					reg("test/bar.txt", "testbartestbar"),
					directory("test2/"),
					link("test2/bazlink", "foo.txt"), // modified
				)),
				checkVerifyInvalidTOCEntryFail("test/bar.txt"),
				checkVerifyBrokenContentFail("test/bar.txt"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Get original tar file and chunk digests
			dgstMap := make(map[string]digest.Digest)
			tarBlob := tt.tarInit(t, dgstMap)

			rc, tocDigest, err := Build(tarBlob, nil, WithChunkSize(chunkSize))
			if err != nil {
				t.Fatalf("failed to convert stargz: %v", err)
			}
			defer rc.Close()
			buf := new(bytes.Buffer)
			if _, err := io.Copy(buf, rc); err != nil {
				t.Fatalf("failed to copy built stargz blob: %v", err)
			}
			newStargz := buf.Bytes()
			// NoPrefetchLandmark is added during `Bulid`, which is expected behaviour.
			dgstMap[chunkID(NoPrefetchLandmark, 0, int64(len([]byte{landmarkContents})))] = digest.FromBytes([]byte{landmarkContents})

			for _, check := range tt.checks {
				check(t, newStargz, tocDigest, dgstMap)
			}
		})
	}
}

// checkStargzTOC checks the TOC JSON of the passed stargz has the expected
// digest and contains valid chunks. It walks all entries in the stargz and
// checks all chunk digests stored to the TOC JSON match the actual contents.
func checkStargzTOC(t *testing.T, sgzData []byte, tocDigest digest.Digest, dgstMap map[string]digest.Digest) {
	sgz, err := stargz.Open(io.NewSectionReader(bytes.NewReader(sgzData), 0, int64(len(sgzData))))
	if err != nil {
		t.Errorf("failed to parse converted stargz: %v", err)
		return
	}
	digestMapTOC, err := listDigests(io.NewSectionReader(
		bytes.NewReader(sgzData), 0, int64(len(sgzData))))
	if err != nil {
		t.Fatalf("failed to list digest: %v", err)
	}
	found := make(map[string]bool)
	for id := range dgstMap {
		found[id] = false
	}
	zr, err := gzip.NewReader(bytes.NewReader(sgzData))
	if err != nil {
		t.Fatalf("failed to decompress converted stargz: %v", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err != nil {
			if err != io.EOF {
				t.Errorf("failed to read tar entry: %v", err)
				return
			}
			break
		}
		if h.Name == stargz.TOCTarName {
			// Check the digest of TOC JSON based on the actual contents
			// It's sure that TOC JSON exists in this archive because
			// stargz.Open succeeded.
			dgstr := digest.Canonical.Digester()
			if _, err := io.Copy(dgstr.Hash(), tr); err != nil {
				t.Fatalf("failed to calculate digest of TOC JSON: %v",
					err)
			}
			if dgstr.Digest() != tocDigest {
				t.Errorf("invalid TOC JSON %q; want %q", tocDigest, dgstr.Digest())
			}
			continue
		}
		if _, ok := sgz.Lookup(strings.TrimSuffix(h.Name, "/")); !ok {
			t.Errorf("lost stargz entry %q in the converted TOC", h.Name)
			return
		}
		var n int64
		for n < h.Size {
			ce, ok := sgz.ChunkEntryForOffset(h.Name, n)
			if !ok {
				t.Errorf("lost chunk %q(offset=%d) in the converted TOC",
					h.Name, n)
				return
			}

			// Get the original digest to make sure the file contents are kept unchanged
			// from the original tar, during the whole conversion steps.
			id := chunkID(h.Name, n, ce.ChunkSize)
			want, ok := dgstMap[id]
			if !ok {
				t.Errorf("Unexpected chunk %q(offset=%d,size=%d): %v",
					h.Name, n, ce.ChunkSize, dgstMap)
				return
			}
			found[id] = true

			// Check the file contents
			dgstr := digest.Canonical.Digester()
			if _, err := io.CopyN(dgstr.Hash(), tr, ce.ChunkSize); err != nil {
				t.Fatalf("failed to calculate digest of %q (offset=%d,size=%d)",
					h.Name, n, ce.ChunkSize)
			}
			if want != dgstr.Digest() {
				t.Errorf("Invalid contents in converted stargz %q: %q; want %q",
					h.Name, dgstr.Digest(), want)
				return
			}

			// Check the digest stored in TOC JSON
			dgstTOC, ok := digestMapTOC[ce.Offset]
			if !ok {
				t.Errorf("digest of %q(offset=%d,size=%d,chunkOffset=%d) isn't registered",
					h.Name, ce.Offset, ce.ChunkSize, ce.ChunkOffset)
			}
			if want != dgstTOC {
				t.Errorf("Invalid digest in TOCEntry %q: %q; want %q",
					h.Name, dgstTOC, want)
				return
			}

			n += ce.ChunkSize
		}
	}

	for id, ok := range found {
		if !ok {
			t.Errorf("required chunk %q not found in the converted stargz: %v", id, found)
		}
	}
}

// checkVerifyTOC checks the verification works for the TOC JSON of the passed
// stargz. It walks all entries in the stargz and checks the verifications for
// all chunks work.
func checkVerifyTOC(t *testing.T, sgzData []byte, tocDigest digest.Digest, dgstMap map[string]digest.Digest) {
	sgz, err := stargz.Open(io.NewSectionReader(bytes.NewReader(sgzData), 0, int64(len(sgzData))))
	if err != nil {
		t.Errorf("failed to parse converted stargz: %v", err)
		return
	}
	ev, err := VerifyStargzTOC(io.NewSectionReader(
		bytes.NewReader(sgzData), 0, int64(len(sgzData))),
		tocDigest,
	)
	if err != nil {
		t.Errorf("failed to verify stargz: %v", err)
		return
	}

	found := make(map[string]bool)
	for id := range dgstMap {
		found[id] = false
	}
	zr, err := gzip.NewReader(bytes.NewReader(sgzData))
	if err != nil {
		t.Fatalf("failed to decompress converted stargz: %v", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err != nil {
			if err != io.EOF {
				t.Errorf("failed to read tar entry: %v", err)
				return
			}
			break
		}
		if h.Name == stargz.TOCTarName {
			continue
		}
		if _, ok := sgz.Lookup(strings.TrimSuffix(h.Name, "/")); !ok {
			t.Errorf("lost stargz entry %q in the converted TOC", h.Name)
			return
		}
		var n int64
		for n < h.Size {
			ce, ok := sgz.ChunkEntryForOffset(h.Name, n)
			if !ok {
				t.Errorf("lost chunk %q(offset=%d) in the converted TOC",
					h.Name, n)
				return
			}

			v, err := ev.Verifier(ce)
			if err != nil {
				t.Errorf("failed to get verifier for %q(offset=%d)", h.Name, n)
			}

			found[chunkID(h.Name, n, ce.ChunkSize)] = true

			// Check the file contents
			if _, err := io.CopyN(v, tr, ce.ChunkSize); err != nil {
				t.Fatalf("failed to get chunk of %q (offset=%d,size=%d)",
					h.Name, n, ce.ChunkSize)
			}
			if !v.Verified() {
				t.Errorf("Invalid contents in converted stargz %q (should be succeeded)",
					h.Name)
				return
			}
			n += ce.ChunkSize
		}
	}

	for id, ok := range found {
		if !ok {
			t.Errorf("required chunk %q not found in the converted stargz: %v", id, found)
		}
	}
}

// checkVerifyInvalidTOCEntryFail checks if misconfigured TOC JSON can be
// detected during the verification and the verification returns an error.
func checkVerifyInvalidTOCEntryFail(filename string) check {
	return func(t *testing.T, sgzData []byte, tocDigest digest.Digest, dgstMap map[string]digest.Digest) {
		funcs := map[string]rewriteFunc{
			"lost digest in a entry": func(t *testing.T, toc *jtoc, sgz *io.SectionReader) {
				for _, e := range toc.Entries {
					if e.Name == filename {
						if e.Type != "reg" && e.Type != "chunk" {
							t.Fatalf("entry %q to break must be regfile or chunk", filename)
						}
						if e.ChunkDigest == "" {
							t.Fatalf("entry %q is already invalid", filename)
						}
						e.ChunkDigest = ""
					}
				}
			},
			"duplicated entry offset": func(t *testing.T, toc *jtoc, sgz *io.SectionReader) {
				var (
					sampleEntry *TOCEntry
					targetEntry *TOCEntry
				)
				for _, e := range toc.Entries {
					if e.Type == "reg" || e.Type == "chunk" {
						if e.Name == filename {
							targetEntry = e
						} else {
							sampleEntry = e
						}
					}
				}
				if sampleEntry == nil {
					t.Fatalf("TOC must contain at least one regfile or chunk entry other than the rewrite target")
				}
				if targetEntry == nil {
					t.Fatalf("rewrite target not found")
				}
				targetEntry.Offset = sampleEntry.Offset
			},
		}

		for name, rFunc := range funcs {
			t.Run(name, func(t *testing.T) {
				newSgz, newTocDigest := rewriteTOCJSON(t, io.NewSectionReader(bytes.NewReader(sgzData), 0, int64(len(sgzData))), rFunc)
				buf := new(bytes.Buffer)
				if _, err := io.Copy(buf, newSgz); err != nil {
					t.Fatalf("failed to get converted stargz")
				}
				isgz := buf.Bytes()
				if _, err := VerifyStargzTOC(io.NewSectionReader(bytes.NewReader(isgz), 0, int64(len(isgz))), newTocDigest); err == nil {
					t.Errorf("must fail for invalid TOC")
					return
				}
			})
		}
	}
}

// checkVerifyInvalidStargzFail checks if the verification detects that the
// given stargz file doesn't match to the expected digest and returns error.
func checkVerifyInvalidStargzFail(invalid *io.SectionReader) check {
	return func(t *testing.T, sgzData []byte, tocDigest digest.Digest, dgstMap map[string]digest.Digest) {
		rc, _, err := Build(invalid, nil, WithChunkSize(chunkSize))
		if err != nil {
			t.Fatalf("failed to convert stargz: %v", err)
		}
		defer rc.Close()
		buf := new(bytes.Buffer)
		if _, err := io.Copy(buf, rc); err != nil {
			t.Fatalf("failed to copy built stargz blob: %v", err)
		}
		mStargz := buf.Bytes()

		_, err = VerifyStargzTOC(io.NewSectionReader(
			bytes.NewReader(mStargz), 0, int64(len(mStargz))), tocDigest)
		if err == nil {
			t.Errorf("must fail for invalid stargz")
			return
		}
	}
}

// checkVerifyBrokenContentFail checks if the verifier detects broken contents
// that doesn't match to the expected digest and returns error.
func checkVerifyBrokenContentFail(filename string) check {
	return func(t *testing.T, sgzData []byte, tocDigest digest.Digest, dgstMap map[string]digest.Digest) {
		// Parse stargz file
		sgz, err := stargz.Open(io.NewSectionReader(bytes.NewReader(sgzData), 0, int64(len(sgzData))))
		if err != nil {
			t.Fatalf("failed to parse converted stargz: %v", err)
			return
		}
		ev, err := VerifyStargzTOC(io.NewSectionReader(
			bytes.NewReader(sgzData), 0, int64(len(sgzData))),
			tocDigest,
		)
		if err != nil {
			t.Fatalf("failed to verify stargz: %v", err)
			return
		}

		// Open the target file
		sr, err := sgz.OpenFile(filename)
		if err != nil {
			t.Fatalf("failed to open file %q", filename)
		}
		ce, ok := sgz.ChunkEntryForOffset(filename, 0)
		if !ok {
			t.Fatalf("lost chunk %q(offset=%d) in the converted TOC", filename, 0)
			return
		}
		if ce.ChunkSize == 0 {
			t.Fatalf("file mustn't be empty")
			return
		}
		data := make([]byte, ce.ChunkSize)
		if _, err := sr.ReadAt(data, ce.ChunkOffset); err != nil {
			t.Errorf("failed to get data of a chunk of %q(offset=%q)",
				filename, ce.ChunkOffset)
		}

		// Check the broken chunk (must fail)
		v, err := ev.Verifier(ce)
		if err != nil {
			t.Fatalf("failed to get verifier for %q", filename)
		}
		broken := append([]byte{^data[0]}, data[1:]...)
		if _, err := io.CopyN(v, bytes.NewReader(broken), ce.ChunkSize); err != nil {
			t.Fatalf("failed to get chunk of %q (offset=%d,size=%d)",
				filename, ce.ChunkOffset, ce.ChunkSize)
		}
		if v.Verified() {
			t.Errorf("verification must fail for broken file chunk %q(org:%q,broken:%q)",
				filename, data, broken)
		}
	}
}

func chunkID(name string, offset, size int64) string {
	return fmt.Sprintf("%s-%d-%d", name, offset, size)
}

type rewriteFunc func(t *testing.T, toc *jtoc, sgz *io.SectionReader)

func rewriteTOCJSON(t *testing.T, sgz *io.SectionReader, rewrite rewriteFunc) (newSgz io.Reader, tocDigest digest.Digest) {
	blob, err := parseStargz(sgz)
	if err != nil {
		t.Fatalf("failed to extract TOC JSON: %v", err)
	}

	rewrite(t, blob.jtoc, sgz)

	tocJSON, err := json.Marshal(blob.jtoc)
	if err != nil {
		t.Fatalf("failed to marshal TOC JSON: %v", err)
	}
	dgstr := digest.Canonical.Digester()
	if _, err := io.CopyN(dgstr.Hash(), bytes.NewReader(tocJSON), int64(len(tocJSON))); err != nil {
		t.Fatalf("failed to calculate digest of TOC JSON: %v", err)
	}
	pr, pw := io.Pipe()
	go func() {
		zw, err := gzip.NewWriterLevel(pw, gzip.BestCompression)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		zw.Extra = []byte("stargz.toc")
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
		t.Fatalf("failed to reset the seek position of stargz: %v", err)
	}
	if _, err := blob.footer.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("failed to reset the seek position of footer: %v", err)
	}
	return io.MultiReader(
		io.LimitReader(sgz, blob.jtocOffset), // Original stargz (before TOC JSON)
		pr,                                   // Rewritten TOC JSON
		blob.footer,                          // Unmodified footer (because tocOffset is unchanged)
	), dgstr.Digest()
}

func regDigest(t *testing.T, name string, contentStr string, digestMap map[string]digest.Digest) appender {
	if digestMap == nil {
		t.Fatalf("digest map mustn't be nil")
	}
	content := []byte(contentStr)

	var n int64
	for n < int64(len(content)) {
		size := int64(chunkSize)
		remain := int64(len(content)) - n
		if remain < size {
			size = remain
		}
		dgstr := digest.Canonical.Digester()
		if _, err := io.CopyN(dgstr.Hash(), bytes.NewReader(content[n:n+size]), size); err != nil {
			t.Fatalf("failed to calculate digest of %q (name=%q,offset=%d,size=%d)",
				string(content[n:n+size]), name, n, size)
		}
		digestMap[chunkID(name, n, size)] = dgstr.Digest()
		n += size
	}

	return func(w *tar.Writer) error {
		if err := w.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Size:     int64(len(content)),
		}); err != nil {
			return err
		}
		if _, err := io.CopyN(w, bytes.NewReader(content), int64(len(content))); err != nil {
			return err
		}
		return nil
	}
}

func listDigests(sgz *io.SectionReader) (map[int64]digest.Digest, error) {
	blob, err := parseStargz(sgz)
	if err != nil {
		return nil, err
	}
	digestMap := make(map[int64]digest.Digest)
	for _, e := range blob.jtoc.Entries {
		if e.Type == "reg" || e.Type == "chunk" {
			if e.Type == "reg" && e.Size == 0 {
				continue // ignores empty file
			}
			if e.ChunkDigest == "" {
				return nil, fmt.Errorf("ChunkDigest of %q(off=%d) not found in TOC JSON",
					e.Name, e.Offset)
			}
			d, err := digest.Parse(e.ChunkDigest)
			if err != nil {
				return nil, err
			}
			digestMap[e.Offset] = d
		}
	}
	return digestMap, nil
}
