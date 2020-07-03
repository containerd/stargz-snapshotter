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
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/google/crfs/stargz"
)

const chunkSize = 3

func TestDigest(t *testing.T) {
	tests := []struct {
		name    string
		tarInit func(t *testing.T, dgstMap map[string]string) (stream io.ReadCloser)
	}{
		{
			name: "no-regfile",
			tarInit: func(t *testing.T, dgstMap map[string]string) (stream io.ReadCloser) {
				return tarStream(
					directory("test/"),
				)
			},
		},
		{
			name: "small-files",
			tarInit: func(t *testing.T, dgstMap map[string]string) (stream io.ReadCloser) {
				return tarStream(
					regDigest(t, "baz.txt", "", dgstMap),
					regDigest(t, "foo.txt", "a", dgstMap),
					directory("test/"),
					regDigest(t, "test/bar.txt", "bbb", dgstMap),
				)
			},
		},
		{
			name: "big-files",
			tarInit: func(t *testing.T, dgstMap map[string]string) (stream io.ReadCloser) {
				return tarStream(
					regDigest(t, "baz.txt", "bazbazbazbazbazbazbaz", dgstMap),
					regDigest(t, "foo.txt", "a", dgstMap),
					directory("test/"),
					regDigest(t, "test/bar.txt", "testbartestbar", dgstMap),
				)
			},
		},
		{
			name: "with-non-regfiles",
			tarInit: func(t *testing.T, dgstMap map[string]string) (stream io.ReadCloser) {
				return tarStream(
					regDigest(t, "baz.txt", "bazbazbazbazbazbazbaz", dgstMap),
					regDigest(t, "foo.txt", "a", dgstMap),
					symLink("barlink", "test/bar.txt"),
					directory("test/"),
					regDigest(t, "test/bar.txt", "testbartestbar", dgstMap),
					directory("test2/"),
					link("test2/bazlink", "baz.txt"),
				)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := new(bytes.Buffer)

			// Get original tar file and chunk digests
			dgstMap := make(map[string]string)
			ts := tt.tarInit(t, dgstMap)

			// Convert tar to stargz
			w := stargz.NewWriter(buf)
			w.ChunkSize = chunkSize
			if err := w.AppendTar(ts); err != nil {
				t.Fatalf("failed to append tar: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("failed to finalize stargz: %v", err)
			}
			orgStargz := buf.Bytes()

			// Rewrite stargz TOC JSON with chunk-level digests
			r, tocDigest, err := NewVerifiableStagz(io.NewSectionReader(
				bytes.NewReader(orgStargz), 0, int64(len(orgStargz))))
			if err != nil {
				t.Errorf("failed to convert stargz: %v", err)
				return
			}
			buf.Reset()
			if _, err := io.Copy(buf, r); err != nil {
				t.Fatalf("failed to get converted stargz")
			}
			newStargz := buf.Bytes()

			// Check the newly created TOC JSON based on the actual contents
			// Walk all entries in the stargz and check all chunks are stored to the TOC JSON
			sgz, err := stargz.Open(io.NewSectionReader(
				bytes.NewReader(newStargz), 0, int64(len(newStargz))))
			if err != nil {
				t.Errorf("failed to parse converted stargz: %v", err)
				return
			}
			digestMapTOC, err := listDigests(io.NewSectionReader(
				bytes.NewReader(newStargz), 0, int64(len(newStargz))))
			if err != nil {
				t.Fatalf("failed to list digest: %v", err)
			}
			found := make(map[string]bool)
			for id := range dgstMap {
				found[id] = false
			}
			zr, err := gzip.NewReader(bytes.NewReader(newStargz))
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
					tocHash := sha256.New()
					if _, err := io.Copy(tocHash, tr); err != nil {
						t.Fatalf("failed to calculate digest of TOC JSON: %v",
							err)
					}
					want := fmt.Sprintf("sha256:%x", tocHash.Sum(nil))
					if want != tocDigest {
						t.Errorf("invalid TOC JSON %q; want %q", tocDigest, want)
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

					// Get the original digest to make sure the file contents are
					// kept unchanged from the original tar, during the whole
					// conversion steps.
					id := chunkID(h.Name, n, ce.ChunkSize)
					want, ok := dgstMap[id]
					if !ok {
						t.Errorf("Unexpected chunk %q(offset=%d,size=%d): %v",
							h.Name, n, ce.ChunkSize, dgstMap)
						return
					}
					found[id] = true

					// Check the file contents
					digest := sha256.New()
					if _, err := io.CopyN(digest, tr, ce.ChunkSize); err != nil {
						t.Fatalf("failed to calculate digest of %q (offset=%d,size=%d)",
							h.Name, n, ce.ChunkSize)
					}
					dgst := fmt.Sprintf("sha256:%x", digest.Sum(nil))
					if want != dgst {
						t.Errorf("Invalid contents in converted stargz %q: %q; want %q",
							h.Name, dgst, want)
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
		})
	}
}

func chunkID(name string, offset, size int64) string {
	return fmt.Sprintf("%s-%d-%d", name, offset, size)
}

type appender func(w *tar.Writer) error

func tarStream(appenders ...appender) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		for _, f := range appenders {
			if err := f(tw); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		if err := tw.Close(); err != nil {
			pw.CloseWithError(err)
			return
		}
	}()
	return pr
}

func directory(name string) appender {
	if !strings.HasSuffix(name, "/") {
		panic(fmt.Sprintf("dir %q must have suffix /", name))
	}
	return func(w *tar.Writer) error {
		return w.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir,
			Name:     name,
		})
	}
}

func regDigest(t *testing.T, name string, contentStr string, digestMap map[string]string) appender {
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
		digest := sha256.New()
		if _, err := io.CopyN(digest, bytes.NewReader(content[n:n+size]), size); err != nil {
			t.Fatalf("failed to calculate digest of %q (name=%q,offset=%d,size=%d)",
				string(content[n:n+size]), name, n, size)
		}
		digestMap[chunkID(name, n, size)] = fmt.Sprintf("sha256:%x", digest.Sum(nil))
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

func link(name string, linkname string) appender {
	return func(w *tar.Writer) error {
		return w.WriteHeader(&tar.Header{
			Typeflag: tar.TypeLink,
			Name:     name,
			Linkname: linkname,
		})
	}
}

func symLink(name string, linkname string) appender {
	return func(w *tar.Writer) error {
		return w.WriteHeader(&tar.Header{
			Typeflag: tar.TypeSymlink,
			Name:     name,
			Linkname: linkname,
		})
	}
}

func listDigests(sgz *io.SectionReader) (map[int64]string, error) {
	toc, _, _, err := extractTOCJSON(sgz)
	if err != nil {
		return nil, err
	}
	digestMap := make(map[int64]string)
	for _, e := range toc.Entries {
		if e.Type == "reg" || e.Type == "chunk" {
			if e.Type == "reg" && e.Size == 0 {
				continue // ignores empty file
			}
			if e.ChunkDigest == "" {
				return nil, fmt.Errorf("ChunkDigest of %q(off=%d) not found in TOC JSON",
					e.Name, e.Offset)
			}
			digestMap[e.Offset] = e.ChunkDigest
		}
	}
	return digestMap, nil
}
