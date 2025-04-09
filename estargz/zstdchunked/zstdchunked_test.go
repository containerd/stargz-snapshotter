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

package zstdchunked

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"sort"
	"testing"

	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/klauspost/compress/zstd"
)

// TestZstdChunked tests zstd:chunked
func TestZstdChunked(t *testing.T) {
	estargz.CompressionTestSuite(t,
		zstdControllerWithLevel(zstd.SpeedFastest),
		zstdControllerWithLevel(zstd.SpeedDefault),
		zstdControllerWithLevel(zstd.SpeedBetterCompression),
		// zstdControllerWithLevel(zstd.SpeedBestCompression), // consumes too much memory to pass on CI
	)
}

func zstdControllerWithLevel(compressionLevel zstd.EncoderLevel) estargz.TestingControllerFactory {
	return func() estargz.TestingController {
		return &zstdController{&Compressor{CompressionLevel: compressionLevel}, &Decompressor{}}
	}
}

type zstdController struct {
	*Compressor
	*Decompressor
}

func (zc *zstdController) String() string {
	return fmt.Sprintf("zstd_compression_level=%v", zc.CompressionLevel)
}

// TestStream tests the passed zstdchunked blob contains the specified list of streams.
// The last entry of streams must be the offset of footer (len(b) - footerSize).
func (zc *zstdController) TestStreams(t *testing.T, b []byte, streams []int64) {
	t.Logf("got zstd streams (compressed size: %d):", len(b))

	if len(streams) == 0 {
		return // nop
	}

	// We expect the last offset is footer offset.
	// 8 is the size of the zstd skippable frame header + the frame size (see WriteTOCAndFooter)
	sort.Slice(streams, func(i, j int) bool {
		return streams[i] < streams[j]
	})
	streams[len(streams)-1] = streams[len(streams)-1] - 8
	wants := map[int64]struct{}{}
	for _, s := range streams {
		wants[s] = struct{}{}
	}

	magicLen := 4 // length of magic bytes and skippable frame magic bytes
	zoff := 0
	numStreams := 0
	for {
		if len(b) <= zoff {
			break
		} else if len(b)-zoff <= magicLen {
			t.Fatalf("invalid frame size %d is too small", len(b)-zoff)
		}
		delete(wants, int64(zoff)) // offset found
		remainingFrames := b[zoff:]

		// Check if zoff points to the beginning of a frame
		if !bytes.Equal(remainingFrames[:magicLen], zstdFrameMagic) {
			if !bytes.Equal(remainingFrames[:magicLen], skippableFrameMagic) {
				t.Fatalf("frame must start from magic bytes; but %x",
					remainingFrames[:magicLen])
			}
		}
		searchBase := magicLen
		nextMagicIdx := nextIndex(remainingFrames[searchBase:], zstdFrameMagic)
		nextSkippableIdx := nextIndex(remainingFrames[searchBase:], skippableFrameMagic)
		nextFrame := len(remainingFrames)
		for _, i := range []int{nextMagicIdx, nextSkippableIdx} {
			if 0 < i && searchBase+i < nextFrame {
				nextFrame = searchBase + i
			}
		}
		t.Logf("  [%d] at %d in stargz (nextFrame: %d/%d): %v, %v",
			numStreams, zoff, zoff+nextFrame, len(b), nextMagicIdx, nextSkippableIdx)
		zoff += nextFrame
		numStreams++
	}
	if len(wants) != 0 {
		t.Fatalf("some stream offsets not found in the blob: %v", wants)
	}
}

func nextIndex(s1, sub []byte) int {
	for i := 0; i < len(s1); i++ {
		if len(s1)-i < len(sub) {
			return -1
		} else if bytes.Equal(s1[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

func (zc *zstdController) DiffIDOf(t *testing.T, b []byte) string {
	h := sha256.New()
	zr, err := zstd.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("diffIDOf(zstd): %v", err)
	}
	defer zr.Close()
	if _, err := io.Copy(h, zr); err != nil {
		t.Fatalf("diffIDOf(zstd).Copy: %v", err)
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}

// Tests footer encoding, size, and parsing of zstd:chunked.
func TestZstdChunkedFooter(t *testing.T) {
	max := int64(200000)
	for off := int64(0); off <= max; off += 1023 {
		size := max - off
		checkZstdChunkedFooter(t, off, size, size/2)
	}
}

func checkZstdChunkedFooter(t *testing.T, off, size, cSize int64) {
	footer := zstdFooterBytes(uint64(off), uint64(size), uint64(cSize))
	if len(footer) != FooterSize {
		t.Fatalf("for offset %v, footer length was %d, not expected %d. got bytes: %q", off, len(footer), FooterSize, footer)
	}
	gotBlobPayloadSize, gotOff, gotSize, err := (&Decompressor{}).ParseFooter(footer)
	if err != nil {
		t.Fatalf("failed to parse footer for offset %d, footer: %x: err: %v",
			off, footer, err)
	}
	if gotBlobPayloadSize != off-8 {
		// 8 is the size of the zstd skippable frame header + the frame size (see WriteTOCAndFooter)
		t.Fatalf("ParseFooter(footerBytes(offset %d)) = blobPayloadSize %d; want %d", off, gotBlobPayloadSize, off-8)
	}
	if gotOff != off {
		t.Fatalf("ParseFooter(footerBytes(offset %d)) = off %d; want %d", off, gotOff, off)
	}
	if gotSize != cSize {
		t.Fatalf("ParseFooter(footerBytes(offset %d)) = size %d; want %d", off, gotSize, cSize)
	}
}
