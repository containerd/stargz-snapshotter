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

/*
   Copyright 2019 The Go Authors. All rights reserved.
   Use of this source code is governed by a BSD-style
   license that can be found in the LICENSE file.
*/

package estargz

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"testing"
)

// TestGzipEStargz tests gzip-based eStargz
func TestGzipEStargz(t *testing.T) {
	testRunner := &TestRunner{
		TestingT: t,
		Runner: func(testingT TestingT, name string, run func(t TestingT)) {
			tt, ok := testingT.(*testing.T)
			if !ok {
				testingT.Fatal("TestingT is not a *testing.T")
				return
			}

			tt.Run(name, func(t *testing.T) {
				run(t)
			})
		},
	}

	CompressionTestSuite(testRunner,
		gzipControllerWithLevel(gzip.NoCompression),
		gzipControllerWithLevel(gzip.BestSpeed),
		gzipControllerWithLevel(gzip.BestCompression),
		gzipControllerWithLevel(gzip.DefaultCompression),
		gzipControllerWithLevel(gzip.HuffmanOnly),
	)
}

func gzipControllerWithLevel(compressionLevel int) TestingControllerFactory {
	return func() TestingController {
		return &gzipController{&GzipCompressor{compressionLevel}, &GzipDecompressor{}}
	}
}

type gzipController struct {
	*GzipCompressor
	*GzipDecompressor
}

func (gc *gzipController) String() string {
	return fmt.Sprintf("gzip_compression_level=%v", gc.compressionLevel)
}

// TestStream tests the passed estargz blob contains the specified list of streams.
func (gc *gzipController) TestStreams(t TestingT, b []byte, streams []int64) {
	CheckGzipHasStreams(t, b, streams)
}

func (gc *gzipController) DiffIDOf(t TestingT, b []byte) string {
	return GzipDiffIDOf(t, b)
}

// Tests footer encoding, size, and parsing of gzip-based eStargz.
func TestGzipFooter(t *testing.T) {
	for off := int64(0); off <= 200000; off += 1023 {
		checkFooter(t, off)
		checkLegacyFooter(t, off)
	}
}

// TODO: check fallback
func checkFooter(t *testing.T, off int64) {
	footer := gzipFooterBytes(off)
	if len(footer) != FooterSize {
		t.Fatalf("for offset %v, footer length was %d, not expected %d. got bytes: %q", off, len(footer), FooterSize, footer)
	}
	_, got, _, err := (&GzipDecompressor{}).ParseFooter(footer)
	if err != nil {
		t.Fatalf("failed to parse footer for offset %d, footer: %x: err: %v",
			off, footer, err)
	}
	if got != off {
		t.Fatalf("ParseFooter(footerBytes(offset %d)) = %d; want %d", off, got, off)
	}
}

func checkLegacyFooter(t *testing.T, off int64) {
	footer := legacyFooterBytes(off)
	if len(footer) != legacyFooterSize {
		t.Fatalf("for offset %v, footer length was %d, not expected %d. got bytes: %q", off, len(footer), legacyFooterSize, footer)
	}
	_, got, _, err := (&LegacyGzipDecompressor{}).ParseFooter(footer)
	if err != nil {
		t.Fatalf("failed to parse legacy footer for offset %d, footer: %x: err: %v",
			off, footer, err)
	}
	if got != off {
		t.Fatalf("ParseFooter(legacyFooterBytes(offset %d)) = %d; want %d", off, got, off)
	}
}

// TestGzipParseFooterInvalidExtra verifies that ParseFooter returns an error
// (instead of panicking) when the gzip Extra field is empty or too short.
func TestGzipParseFooterInvalidExtra(t *testing.T) {
	tests := []struct {
		name  string
		extra []byte
	}{
		{"nil extra", nil},
		{"empty extra", []byte{}},
		{"short extra (2 bytes)", []byte{0x00, 0x01}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build a FooterSize gzip block with the given Extra field.
			// Pad the compressed body so the total is exactly FooterSize bytes.
			buf := new(bytes.Buffer)
			gz, _ := gzip.NewWriterLevel(buf, gzip.NoCompression)
			gz.Extra = tt.extra
			gz.Close()

			// Pad or truncate to FooterSize so the length check passes.
			footer := make([]byte, FooterSize)
			copy(footer, buf.Bytes())

			_, _, _, err := (&GzipDecompressor{}).ParseFooter(footer)
			if err == nil {
				t.Fatal("expected error from ParseFooter with invalid extra field, got nil")
			}
		})
	}
}

func legacyFooterBytes(tocOff int64) []byte {
	buf := CreateGzipFooter(fmt.Appendf(nil, "%016xSTARGZ", tocOff))
	if len(buf) != legacyFooterSize {
		panic(fmt.Sprintf("footer buffer = %d, not %d", len(buf), legacyFooterSize))
	}
	return buf
}
