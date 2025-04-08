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

package externaltoc

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"testing"

	"github.com/containerd/stargz-snapshotter/estargz"
)

// TestGzipEStargz tests gzip-based external TOC eStargz
func TestGzipEStargz(t *testing.T) {
	estargz.CompressionTestSuite(t,
		gzipControllerWithLevel(gzip.NoCompression),
		gzipControllerWithLevel(gzip.BestSpeed),
		gzipControllerWithLevel(gzip.BestCompression),
		gzipControllerWithLevel(gzip.DefaultCompression),
		gzipControllerWithLevel(gzip.HuffmanOnly),
	)
}

func gzipControllerWithLevel(compressionLevel int) estargz.TestingControllerFactory {
	return func() estargz.TestingController {
		compressor := NewGzipCompressorWithLevel(compressionLevel)
		decompressor := NewGzipDecompressor(func() ([]byte, error) {
			buf := new(bytes.Buffer)
			if _, err := compressor.WriteTOCTo(buf); err != nil {
				return nil, err
			}
			return buf.Bytes(), nil
		})
		return &gzipController{compressor, decompressor}
	}
}

type gzipController struct {
	*GzipCompressor
	*GzipDecompressor
}

func (gc *gzipController) String() string {
	return fmt.Sprintf("externaltoc_gzip_compression_level=%v", gc.compressionLevel)
}

// TestStream tests the passed estargz blob contains the specified list of streams.
func (gc *gzipController) TestStreams(t *testing.T, b []byte, streams []int64) {
	estargz.CheckGzipHasStreams(t, b, streams)
}

func (gc *gzipController) DiffIDOf(t *testing.T, b []byte) string {
	return estargz.GzipDiffIDOf(t, b)
}

// Tests footer encoding, size, and parsing of gzip-based eStargz.
func TestGzipFooter(t *testing.T) {
	footer, err := gzipFooterBytes()
	if err != nil {
		t.Fatalf("failed gzipFooterBytes: %v", err)
	}
	if len(footer) != FooterSize {
		t.Fatalf("footer length was %d, not expected %d. got bytes: %q", len(footer), FooterSize, footer)
	}
	_, gotTOCOffset, _, err := (&GzipDecompressor{}).ParseFooter(footer)
	if err != nil {
		t.Fatalf("failed to parse footer, footer: %x: err: %v", footer, err)
	}
	if gotTOCOffset != -1 {
		t.Fatalf("ParseFooter(footerBytes) must return -1 for external toc but got %d", gotTOCOffset)
	}
}
