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

package db

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/metadata"
	"github.com/goccy/go-json"
	digest "github.com/opencontainers/go-digest"
	"github.com/rs/xid"
	bolt "go.etcd.io/bbolt"
	errbolt "go.etcd.io/bbolt/errors"
	"golang.org/x/sync/errgroup"
)

// reader stores filesystem metadata parsed from eStargz to metadata DB
// and provides methods to read them.
type reader struct {
	db        *bolt.DB
	fsID      string
	rootID    uint32
	tocDigest digest.Digest
	sr        *io.SectionReader

	curID   uint32
	curIDMu sync.Mutex
	initG   *errgroup.Group

	decompressor metadata.Decompressor
}

func (r *reader) nextID() (uint32, error) {
	r.curIDMu.Lock()
	defer r.curIDMu.Unlock()
	if r.curID == math.MaxUint32 {
		return 0, fmt.Errorf("sequence id too large")
	}
	r.curID++
	return r.curID, nil
}

// NewReader parses an eStargz and stores filesystem metadata to
// the provided DB.
func NewReader(db *bolt.DB, sr *io.SectionReader, opts ...metadata.Option) (metadata.Reader, error) {
	var rOpts metadata.Options
	for _, o := range opts {
		if err := o(&rOpts); err != nil {
			return nil, fmt.Errorf("failed to apply option: %w", err)
		}
	}

	gzipCompressors := []metadata.Decompressor{new(estargz.GzipDecompressor), new(estargz.LegacyGzipDecompressor)}
	decompressors := append(gzipCompressors, rOpts.Decompressors...)

	// Determine the size to fetch. Try to fetch as many bytes as possible.
	fetchSize := maxFooterSize(sr.Size(), decompressors...)
	if maybeTocOffset := rOpts.TOCOffset; maybeTocOffset > fetchSize {
		if maybeTocOffset > sr.Size() {
			return nil, fmt.Errorf("blob size %d is smaller than the toc offset", sr.Size())
		}
		fetchSize = sr.Size() - maybeTocOffset
	}

	start := time.Now() // before getting layer footer
	footer := make([]byte, fetchSize)
	if _, err := sr.ReadAt(footer, sr.Size()-fetchSize); err != nil {
		return nil, fmt.Errorf("error reading footer: %v", err)
	}
	if rOpts.Telemetry != nil && rOpts.Telemetry.GetFooterLatency != nil {
		rOpts.Telemetry.GetFooterLatency(start)
	}

	var errs []error
	var tocR io.ReadCloser
	var decompressor metadata.Decompressor
	for _, d := range decompressors {
		fSize := d.FooterSize()
		fOffset := positive(int64(len(footer)) - fSize)
		maybeTocBytes := footer[:fOffset]
		_, tocOffset, tocSize, err := d.ParseFooter(footer[fOffset:])
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if tocOffset >= 0 && tocSize <= 0 {
			tocSize = sr.Size() - tocOffset - fSize
		}
		if tocOffset >= 0 && tocSize < int64(len(maybeTocBytes)) {
			maybeTocBytes = maybeTocBytes[:tocSize]
		}
		tocR, err = decompressTOC(d, sr, tocOffset, tocSize, maybeTocBytes, rOpts)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		decompressor = d
		break
	}

	allErr := errors.Join(errs...)
	if tocR == nil {
		if allErr == nil {
			return nil, fmt.Errorf("failed to get the reader of TOC: unknown")
		}
		return nil, fmt.Errorf("failed to get the reader of TOC: %w", allErr)
	}
	defer tocR.Close()
	r := &reader{sr: sr, db: db, initG: new(errgroup.Group), decompressor: decompressor}
	if err := r.init(tocR, rOpts); err != nil {
		return nil, fmt.Errorf("failed to initialize matadata: %w", err)
	}
	return r, nil
}

func maxFooterSize(blobSize int64, decompressors ...metadata.Decompressor) (res int64) {
	for _, d := range decompressors {
		if s := d.FooterSize(); res < s && s <= blobSize {
			res = s
		}
	}
	return
}

func decompressTOC(d metadata.Decompressor, sr *io.SectionReader, tocOff, tocSize int64, tocBytes []byte, opts metadata.Options) (io.ReadCloser, error) {
	if tocOff < 0 {
		// This means that TOC isn't contained in the blob.
		// We pass nil reader to DecompressTOC and expect that it acquires TOC from
		// the external location.
		start := time.Now()
		tocR, err := d.DecompressTOC(nil)
		if err != nil {
			return nil, err
		}
		if opts.Telemetry != nil && opts.Telemetry.GetTocLatency != nil {
			opts.Telemetry.GetTocLatency(start)
		}
		return tocR, nil
	}
	if len(tocBytes) > 0 {
		start := time.Now() // before getting TOC
		tocR, err := d.DecompressTOC(bytes.NewReader(tocBytes))
		if err == nil {
			if opts.Telemetry != nil && opts.Telemetry.GetTocLatency != nil {
				opts.Telemetry.GetTocLatency(start)
			}
			return tocR, nil
		}
	}

	start := time.Now() // before getting TOC
	tocBytes = make([]byte, tocSize)
	if _, err := sr.ReadAt(tocBytes, tocOff); err != nil {
		return nil, fmt.Errorf("error reading %d byte TOC targz: %v", len(tocBytes), err)
	}
	r, err := d.DecompressTOC(bytes.NewReader(tocBytes))
	if err != nil {
		return nil, err
	}
	if opts.Telemetry != nil && opts.Telemetry.GetTocLatency != nil {
		opts.Telemetry.GetTocLatency(start)
	}
	return r, nil
}

// RootID returns ID of the root node.
func (r *reader) RootID() uint32 {
	return r.rootID
}

func (r *reader) TOCDigest() digest.Digest {
	return r.tocDigest
}

// Clone returns a new reader identical to the current reader
// but uses the provided section reader for retrieving file paylaods.
func (r *reader) Clone(sr *io.SectionReader) (metadata.Reader, error) {
	if err := r.waitInit(); err != nil {
		return nil, err
	}
	return &reader{
		db:           r.db,
		fsID:         r.fsID,
		rootID:       r.rootID,
		sr:           sr,
		initG:        new(errgroup.Group),
		decompressor: r.decompressor,
	}, nil
}

func (r *reader) init(decompressedR io.Reader, rOpts metadata.Options) (retErr error) {
	start := time.Now() // before parsing TOC JSON

	// Initialize root node
	var ok bool
	for i := 0; i < 100; i++ {
		fsID := xid.New().String()
		if err := r.initRootNode(fsID); err != nil {
			if errors.Is(err, errbolt.ErrBucketExists) {
				continue // try with another id
			}
			return fmt.Errorf("failed to initialize root node %q: %w", fsID, err)
		}
		ok = true
		break
	}
	if !ok {
		return fmt.Errorf("failed to get a unique id for metadata reader")
	}

	f, err := os.CreateTemp("", "")
	if err != nil {
		return err
	}
	closeFunc := func() error {
		name := f.Name()
		var errs []error
		if err := f.Close(); err != nil {
			errs = append(errs, err)
		}
		if err := os.Remove(name); err != nil {
			errs = append(errs, err)
		}
		return errors.Join(errs...)
	}
	defer func() {
		if retErr != nil {
			if err := closeFunc(); err != nil {
				retErr = errors.Join(retErr, err)
				return
			}
		}
	}()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	dgstr := digest.Canonical.Digester()
	if _, err := io.Copy(f, io.TeeReader(decompressedR, dgstr.Hash())); err != nil {
		return fmt.Errorf("failed to read TOC: %w", err)
	}
	r.tocDigest = dgstr.Digest()

	// Initialize file metadata in background. All operations refer to these metadata must wait
	// until this initialization ends.
	r.initG.Go(func() error {
		defer closeFunc()
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := r.initNodes(f); err != nil {
			return err
		}
		if rOpts.Telemetry != nil && rOpts.Telemetry.DeserializeTocLatency != nil {
			rOpts.Telemetry.DeserializeTocLatency(start)
		}
		return nil
	})
	return nil
}

func (r *reader) initRootNode(fsID string) error {
	return r.db.Batch(func(tx *bolt.Tx) (err error) {
		filesystems, err := tx.CreateBucketIfNotExists(bucketKeyFilesystems)
		if err != nil {
			return err
		}
		lbkt, err := filesystems.CreateBucket([]byte(fsID))
		if err != nil {
			return err
		}
		r.fsID = fsID
		if _, err := lbkt.CreateBucket(bucketKeyMetadata); err != nil {
			return err
		}
		if _, err := lbkt.CreateBucket(bucketKeyStream); err != nil {
			return err
		}
		nodes, err := lbkt.CreateBucket(bucketKeyNodes)
		if err != nil {
			return err
		}
		rootID, err := r.nextID()
		if err != nil {
			return err
		}
		rootBucket, err := nodes.CreateBucket(encodeID(rootID))
		if err != nil {
			return err
		}
		if err := writeAttr(rootBucket, &metadata.Attr{
			Mode:    os.ModeDir | 0755,
			NumLink: 2, // The directory itself(.) and the parent link to this directory.
		}); err != nil {
			return err
		}
		r.rootID = rootID
		return err
	})
}

func (r *reader) initNodes(tr io.Reader) error {
	dec := json.NewDecoder(tr)
	for {
		t, err := dec.Token()
		if err != nil {
			return fmt.Errorf("failed to get JSON token: %w", err)
		}
		if ele, ok := t.(string); ok {
			if ele == "version" {
				continue
			}
			if ele == "entries" {
				continue
			}
		}
		if de, ok := t.(json.Delim); ok {
			if de.String() == "[" {
				break
			}
		}
	}
	md := make(map[uint32]*metadataEntry)
	st := make(map[int64]map[int64]uint32)
	if err := r.db.Batch(func(tx *bolt.Tx) (err error) {
		nodes, err := getNodes(tx, r.fsID)
		if err != nil {
			return err
		}
		nodes.FillPercent = 1.0 // we only do sequential write to this bucket
		var wantNextOffsetID []uint32
		var lastEntBucketID uint32
		var lastEntSize int64
		var attr metadata.Attr
		var ent estargz.TOCEntry
		var fileDigest string
		for dec.More() {
			resetEnt(&ent)
			if err := dec.Decode(&ent); err != nil {
				return err
			}
			ent.Name = cleanEntryName(ent.Name)
			if ent.Type == "chunk" {
				if lastEntBucketID == 0 {
					return fmt.Errorf("chunk entry must not be the topmost")
				}
				if ent.ChunkSize == 0 { // last chunk in this file
					ent.ChunkSize = lastEntSize - ent.ChunkOffset
				}
			}
			if ent.ChunkSize == 0 && ent.Size != 0 {
				ent.ChunkSize = ent.Size
			}
			if ent.Type != "chunk" {
				var id uint32
				var b *bolt.Bucket
				if ent.Type == "hardlink" {
					id, err = getIDByName(md, ent.LinkName, r.rootID)
					if err != nil {
						return fmt.Errorf("%q is a hardlink but cannot get link destination %q: %w", ent.Name, ent.LinkName, err)
					}
					b, err = getNodeBucketByID(nodes, id)
					if err != nil {
						return fmt.Errorf("cannot get hardlink destination %q ==> %q (%d): %w", ent.Name, ent.LinkName, id, err)
					}
					numLink, _ := binary.Varint(b.Get(bucketKeyNumLink))
					if err := putInt(b, bucketKeyNumLink, numLink+1); err != nil {
						return fmt.Errorf("cannot put NumLink of %q ==> %q: %w", ent.Name, ent.LinkName, err)
					}
				} else {
					// Write node bucket
					var found bool
					if ent.Type == "dir" {
						// Check if this directory is already created, if so overwrite it.
						id, err = getIDByName(md, ent.Name, r.rootID)
						if err == nil {
							b, err = getNodeBucketByID(nodes, id)
							if err != nil {
								return fmt.Errorf("failed to get directory bucket %d: %w", id, err)
							}
							found = true
							ent.NumLink = readNumLink(b)
						}
					}
					if !found {
						// No existing node. Create a new one.
						id, err = r.nextID()
						if err != nil {
							return err
						}
						b, err = nodes.CreateBucket(encodeID(id))
						if err != nil {
							return err
						}
						ent.NumLink = 1 // at least the parent dir references this directory.
						if ent.Type == "dir" {
							ent.NumLink++ // at least "." references this directory.
						}
					}
					if err := writeAttr(b, attrFromTOCEntry(&ent, &attr)); err != nil {
						return fmt.Errorf("failed to set attr to %d(%q): %w", id, ent.Name, err)
					}
				}

				pdirName := parentDir(ent.Name)
				pid, pb, err := r.getOrCreateDir(nodes, md, pdirName, r.rootID)
				if err != nil {
					return fmt.Errorf("failed to create parent directory %q of %q: %w", pdirName, ent.Name, err)
				}
				if err := setChild(md, pb, pid, path.Base(ent.Name), id, ent.Type == "dir"); err != nil {
					return err
				}

				if ent.Offset > 0 && ent.InnerOffset == 0 && len(wantNextOffsetID) > 0 {
					for _, i := range wantNextOffsetID {
						if md[i] == nil {
							md[i] = &metadataEntry{}
						}
						md[i].nextOffset = ent.Offset
					}
					wantNextOffsetID = nil
				}
				if ent.Type == "reg" && ent.Size > 0 {
					wantNextOffsetID = append(wantNextOffsetID, id)
				}

				if ent.Type == "reg" {
					fileDigest = ent.Digest
				}
				lastEntSize = ent.Size
				lastEntBucketID = id
			}
			if (ent.Type == "reg" && ent.Size > 0) || (ent.Type == "chunk" && ent.ChunkSize > 0) {
				if md[lastEntBucketID] == nil {
					md[lastEntBucketID] = &metadataEntry{}
				}
				ce := chunkEntry{ent.Offset, ent.ChunkOffset, ent.ChunkSize, ent.ChunkDigest, fileDigest, ent.InnerOffset}
				md[lastEntBucketID].chunks = append(md[lastEntBucketID].chunks, ce)
				if _, ok := st[ent.Offset]; !ok {
					st[ent.Offset] = make(map[int64]uint32)
				}
				st[ent.Offset][ent.InnerOffset] = lastEntBucketID
			}
		}
		if len(wantNextOffsetID) > 0 {
			for _, i := range wantNextOffsetID {
				if md[i] == nil {
					md[i] = &metadataEntry{}
				}
				md[i].nextOffset = r.sr.Size()
			}
		}
		return nil
	}); err != nil {
		return err
	}

	for mdK, d := range md {
		for cK, ce := range d.chunks {
			if len(st[ce.offset]) == 1 {
				for ioff := range st[ce.offset] {
					if ioff == 0 {
						// This stream contains only 1 chunk with innerOffset=0. No need to record innerOffsets.
						md[mdK].chunks[cK].innerOffset = -1 // indicates no following chunks in this stream.
					}
					break
				}
			}
		}
	}

	addendum := make([]struct {
		id []byte
		md *metadataEntry
	}, len(md))
	i := 0
	for id, d := range md {
		addendum[i].id, addendum[i].md = encodeID(id), d
		i++
	}
	sort.Slice(addendum, func(i, j int) bool {
		return bytes.Compare(addendum[i].id, addendum[j].id) < 0
	})
	if err := r.db.Batch(func(tx *bolt.Tx) (err error) {
		meta, err := getMetadata(tx, r.fsID)
		if err != nil {
			return err
		}
		meta.FillPercent = 1.0 // we only do sequential write to this bucket
		for _, m := range addendum {
			md, err := meta.CreateBucket(m.id)
			if err != nil {
				return err
			}
			if err := writeMetadataEntry(md, m.md); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}

	addendumStream := make([]struct {
		offset []byte
		st     map[int64]uint32
	}, len(st))
	i = 0
	for off, s := range st {
		singleStream := false
		if len(s) == 1 {
			for ioff := range s {
				if ioff == 0 {
					singleStream = true
				}
				break
			}
		}
		if singleStream {
			continue // This stream contains only 1 chunk with innerOffset=0. No need to record.
		}
		offKey, err := encodeInt(off)
		if err != nil {
			return err
		}
		addendumStream[i].offset, addendumStream[i].st = offKey, s
		i++
	}
	addendumStream = addendumStream[:i]
	if len(addendumStream) > 0 {
		sort.Slice(addendumStream, func(i, j int) bool {
			return bytes.Compare(addendumStream[i].offset, addendumStream[j].offset) < 0
		})
		if err := r.db.Batch(func(tx *bolt.Tx) (err error) {
			stream, err := getStream(tx, r.fsID)
			if err != nil {
				return err
			}
			stream.FillPercent = 1.0 // we only do sequential write to this bucket
			for _, s := range addendumStream {
				stbkt, err := stream.CreateBucket(s.offset)
				if err != nil {
					return err
				}
				for innerOffset, nodeid := range s.st {
					iOffKey, err := encodeInt(innerOffset)
					if err != nil {
						return err
					}
					if err := stbkt.Put(iOffKey, encodeID(nodeid)); err != nil {
						return fmt.Errorf("failed to put inner offset info of %d: %w", nodeid, err)
					}
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (r *reader) getOrCreateDir(nodes *bolt.Bucket, md map[uint32]*metadataEntry, d string, rootID uint32) (id uint32, b *bolt.Bucket, err error) {
	id, err = getIDByName(md, d, rootID)
	if err != nil {
		id, err = r.nextID()
		if err != nil {
			return 0, nil, err
		}
		b, err = nodes.CreateBucket(encodeID(id))
		if err != nil {
			return 0, nil, err
		}
		attr := &metadata.Attr{
			Mode:    os.ModeDir | 0755,
			NumLink: 2, // The directory itself(.) and the parent link to this directory.
		}
		if err := writeAttr(b, attr); err != nil {
			return 0, nil, err
		}
		if d != "" {
			pid, pb, err := r.getOrCreateDir(nodes, md, parentDir(d), rootID)
			if err != nil {
				return 0, nil, err
			}
			if err := setChild(md, pb, pid, path.Base(d), id, true); err != nil {
				return 0, nil, err
			}
		}
	} else {
		b, err = getNodeBucketByID(nodes, id)
		if err != nil {
			return 0, nil, fmt.Errorf("failed to get dir bucket %d: %w", id, err)
		}
	}
	return id, b, nil
}

func (r *reader) waitInit() error {
	// TODO: add timeout
	if err := r.initG.Wait(); err != nil {
		return fmt.Errorf("initialization failed: %w", err)
	}
	return nil
}

func (r *reader) view(fn func(tx *bolt.Tx) error) error {
	if err := r.waitInit(); err != nil {
		return err
	}
	return r.db.View(func(tx *bolt.Tx) error {
		return fn(tx)
	})
}

func (r *reader) update(fn func(tx *bolt.Tx) error) error {
	if err := r.waitInit(); err != nil {
		return err
	}
	return r.db.Batch(func(tx *bolt.Tx) error {
		return fn(tx)
	})
}

// Close closes this reader. This removes underlying filesystem metadata as well.
func (r *reader) Close() error {
	return r.update(func(tx *bolt.Tx) (err error) {
		filesystems := tx.Bucket(bucketKeyFilesystems)
		if filesystems == nil {
			return nil
		}
		return filesystems.DeleteBucket([]byte(r.fsID))
	})
}

// GetOffset returns an offset of a node.
func (r *reader) GetOffset(id uint32) (offset int64, _ error) {
	if err := r.view(func(tx *bolt.Tx) error {
		metadataEntries, err := getMetadata(tx, r.fsID)
		if err != nil {
			return fmt.Errorf("metadata bucket of %q not found for searching offset of %d: %w", r.fsID, id, err)
		}
		nodes, err := getNodes(tx, r.fsID)
		if err != nil {
			return err
		}
		b, err := getNodeBucketByID(nodes, id)
		if err != nil {
			return err
		}
		size, _ := binary.Varint(b.Get(bucketKeySize))
		if md, err := getMetadataBucketByID(metadataEntries, id); err == nil {
			chunks, err := readChunks(md, size)
			if err != nil {
				return err
			}
			if len(chunks) > 0 {
				offset = chunks[0].offset
			}
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return
}

// GetAttr returns file attribute of specified node.
func (r *reader) GetAttr(id uint32) (attr metadata.Attr, _ error) {
	if r.rootID == id { // no need to wait for root dir
		if err := r.db.View(func(tx *bolt.Tx) error {
			nodes, err := getNodes(tx, r.fsID)
			if err != nil {
				return fmt.Errorf("nodes bucket of %q not found for sarching attr %d: %w", r.fsID, id, err)
			}
			b, err := getNodeBucketByID(nodes, id)
			if err != nil {
				return fmt.Errorf("failed to get attr bucket %d: %w", id, err)
			}
			return readAttr(b, &attr)
		}); err != nil {
			return metadata.Attr{}, err
		}
		return attr, nil
	}
	if err := r.view(func(tx *bolt.Tx) error {
		nodes, err := getNodes(tx, r.fsID)
		if err != nil {
			return fmt.Errorf("nodes bucket of %q not found for sarching attr %d: %w", r.fsID, id, err)
		}
		b, err := getNodeBucketByID(nodes, id)
		if err != nil {
			return fmt.Errorf("failed to get attr bucket %d: %w", id, err)
		}
		return readAttr(b, &attr)
	}); err != nil {
		return metadata.Attr{}, err
	}
	return
}

// GetChild returns a child node that has the specified base name.
func (r *reader) GetChild(pid uint32, base string) (id uint32, attr metadata.Attr, _ error) {
	if err := r.view(func(tx *bolt.Tx) error {
		metadataEntries, err := getMetadata(tx, r.fsID)
		if err != nil {
			return fmt.Errorf("metadata bucket of %q not found for getting child of %d: %w", r.fsID, pid, err)
		}
		md, err := getMetadataBucketByID(metadataEntries, pid)
		if err != nil {
			return fmt.Errorf("failed to get parent metadata %d: %w", pid, err)
		}
		id, err = readChild(md, base)
		if err != nil {
			return fmt.Errorf("failed to read child %q of %d: %w", base, pid, err)
		}
		nodes, err := getNodes(tx, r.fsID)
		if err != nil {
			return fmt.Errorf("nodes bucket of %q not found for getting child of %d: %w", r.fsID, pid, err)
		}
		child, err := getNodeBucketByID(nodes, id)
		if err != nil {
			return fmt.Errorf("failed to get child bucket %d: %w", id, err)
		}
		return readAttr(child, &attr)
	}); err != nil {
		return 0, metadata.Attr{}, err
	}
	return
}

// ForeachChild calls the specified callback function for each child node.
// When the callback returns non-nil error, this stops the iteration.
func (r *reader) ForeachChild(id uint32, f func(name string, id uint32, mode os.FileMode) bool) error {
	type childInfo struct {
		id   uint32
		mode os.FileMode
	}
	children := make(map[string]childInfo)
	if err := r.view(func(tx *bolt.Tx) error {
		metadataEntries, err := getMetadata(tx, r.fsID)
		if err != nil {
			return fmt.Errorf("nodes bucket of %q not found for getting child of %d: %w", r.fsID, id, err)
		}
		md, err := getMetadataBucketByID(metadataEntries, id)
		if err != nil {
			return nil // no child
		}

		var nodes *bolt.Bucket
		firstName := md.Get(bucketKeyChildName)
		if len(firstName) != 0 {
			firstID := decodeID(md.Get(bucketKeyChildID))
			if nodes == nil {
				nodes, err = getNodes(tx, r.fsID)
				if err != nil {
					return fmt.Errorf("nodes bucket of %q not found for getting children of %d: %w", r.fsID, id, err)
				}
			}
			firstChild, err := getNodeBucketByID(nodes, firstID)
			if err != nil {
				return fmt.Errorf("failed to get first child bucket %d: %w", firstID, err)
			}
			mode, _ := binary.Uvarint(firstChild.Get(bucketKeyMode))
			children[string(firstName)] = childInfo{firstID, os.FileMode(uint32(mode))}
		}

		cbkt := md.Bucket(bucketKeyChildrenExtra)
		if cbkt == nil {
			return nil // no child
		}
		if nodes == nil {
			nodes, err = getNodes(tx, r.fsID)
			if err != nil {
				return fmt.Errorf("nodes bucket of %q not found for getting children of %d: %w", r.fsID, id, err)
			}
		}
		return cbkt.ForEach(func(k, v []byte) error {
			id := decodeID(v)
			child, err := getNodeBucketByID(nodes, id)
			if err != nil {
				return fmt.Errorf("failed to get child bucket %d: %w", id, err)
			}
			mode, _ := binary.Uvarint(child.Get(bucketKeyMode))
			children[string(k)] = childInfo{id, os.FileMode(uint32(mode))}
			return nil
		})
	}); err != nil {
		return err
	}
	for k, e := range children {
		if !f(k, e.id, e.mode) {
			break
		}
	}
	return nil
}

// OpenFileWithPreReader returns a section reader of the specified node.
// When it reads other ranges than required by the returned reader (e.g. when the target range is located in
// a large chunk with innerOffset), these chunks are passed to the callback so that it can be cached for futural use.
func (r *reader) OpenFileWithPreReader(id uint32, preRead func(nid uint32, chunkOffset, chunkSize int64, chunkDigest string, r io.Reader) error) (metadata.File, error) {
	return r.openFile(id, preRead)
}

// OpenFile returns a section reader of the specified node.
func (r *reader) OpenFile(id uint32) (metadata.File, error) {
	return r.openFile(id, nil)
}

func (r *reader) openFile(id uint32, preRead func(id uint32, chunkOffset, chunkSize int64, chunkDigest string, r io.Reader) error) (metadata.File, error) {
	var chunks []chunkEntry
	var size int64

	var nextOffset int64
	if err := r.view(func(tx *bolt.Tx) error {
		nodes, err := getNodes(tx, r.fsID)
		if err != nil {
			return fmt.Errorf("nodes bucket of %q not found for opening %d: %w", r.fsID, id, err)
		}
		b, err := getNodeBucketByID(nodes, id)
		if err != nil {
			return fmt.Errorf("failed to get file bucket %d: %w", id, err)
		}
		size, _ = binary.Varint(b.Get(bucketKeySize))
		m, _ := binary.Uvarint(b.Get(bucketKeyMode))
		if !os.FileMode(uint32(m)).IsRegular() {
			return fmt.Errorf("%q is not a regular file", id)
		}

		metadataEntries, err := getMetadata(tx, r.fsID)
		if err != nil {
			return fmt.Errorf("metadata bucket of %q not found for opening %d: %w", r.fsID, id, err)
		}
		if md, err := getMetadataBucketByID(metadataEntries, id); err == nil {
			chunks, err = readChunks(md, size)
			if err != nil {
				return fmt.Errorf("failed to get chunks: %w", err)
			}
			nextOffset, _ = binary.Varint(md.Get(bucketKeyNextOffset))
		}
		return nil
	}); err != nil {
		return nil, err
	}
	fr := &fileReader{
		r:          r,
		size:       size,
		ents:       chunks,
		nextOffset: nextOffset,
		preRead:    preRead,
	}
	return &file{io.NewSectionReader(fr, 0, size), chunks}, nil
}

type file struct {
	io.ReaderAt
	ents []chunkEntry
}

func (fr *file) ChunkEntryForOffset(offset int64) (off int64, size int64, dgst string, fileDigest string, ok bool) {
	i := sort.Search(len(fr.ents), func(i int) bool {
		e := fr.ents[i]
		return e.chunkOffset >= offset || (offset > e.chunkOffset && offset < e.chunkOffset+e.chunkSize)
	})
	if i == len(fr.ents) {
		return 0, 0, "", "", false
	}
	ci := fr.ents[i]
	return ci.chunkOffset, ci.chunkSize, ci.chunkDigest, ci.fileDigest, true
}

type fileReader struct {
	r          *reader
	size       int64
	ents       []chunkEntry
	nextOffset int64
	preRead    func(id uint32, chunkOffset, chunkSize int64, chunkDigest string, r io.Reader) error
}

// ReadAt reads file payload of this file.
func (fr *fileReader) ReadAt(p []byte, off int64) (n int, err error) {
	if off >= fr.size {
		return 0, io.EOF
	}
	if off < 0 {
		return 0, errors.New("invalid offset")
	}

	var ent chunkEntry
	switch len(fr.ents) {
	case 0:
		return 0, errors.New("no chunk is registered")
	case 1:
		ent = fr.ents[0]
		if ent.chunkOffset > off {
			return 0, fmt.Errorf("no chunk coveres offset %d", off)
		}
	default:
		i := sort.Search(len(fr.ents), func(i int) bool {
			return fr.ents[i].chunkOffset > off
		})
		if i == 0 {
			return 0, fmt.Errorf("no chunk coveres offset %d", off)
		}
		ent = fr.ents[i-1]
	}

	compressedBytesRemain := fr.nextOffset - ent.offset
	bufSize := int(2 << 20)
	if bufSize > int(compressedBytesRemain) {
		bufSize = int(compressedBytesRemain)
	}

	br := bufio.NewReaderSize(io.NewSectionReader(fr.r.sr, ent.offset, compressedBytesRemain), bufSize)
	if _, err := br.Peek(bufSize); err != nil {
		return 0, fmt.Errorf("failed to peek read file payload: %v", err)
	}
	dr, err := fr.r.decompressor.Reader(br)
	if err != nil {
		return 0, fmt.Errorf("fileReader.ReadAt.decompressor.Reader: %v", err)
	}
	defer dr.Close()

	// Stream that doesn't contain multiple chunks is indicated as ent.innerOffset < 0.
	if fr.preRead == nil || ent.innerOffset < 0 {
		base := off - ent.chunkOffset
		if ent.innerOffset > 0 {
			base += ent.innerOffset
		}
		if n, err := io.CopyN(io.Discard, dr, base); n != base || err != nil {
			return 0, fmt.Errorf("discard of %d bytes = %v, %v", base, n, err)
		}
		return io.ReadFull(dr, p)
	}

	var innerChunks []chunkEntryWithID
	if err := fr.r.view(func(tx *bolt.Tx) error {
		innerChunks, err = readInnerChunks(tx, fr.r.fsID, ent.offset)
		return err
	}); err != nil {
		return 0, err
	}
	var found bool
	var nr int64
	var retN int
	var retErr error
	for _, e := range innerChunks {
		// Fully read the previous chunk reader so that the seek position goes at the current chunk offset
		if in, err := io.CopyN(io.Discard, dr, e.innerOffset-nr); err != nil || in != e.innerOffset-nr {
			return 0, fmt.Errorf("discard of remaining %d bytes != %v, %v", e.innerOffset-nr, in, err)
		}
		nr += e.innerOffset - nr
		if e.innerOffset == ent.innerOffset {
			found = true
			base := off - ent.chunkOffset
			if n, err := io.CopyN(io.Discard, dr, base); n != base || err != nil {
				return 0, fmt.Errorf("discard of offset %d bytes != %v, %v", off, n, err)
			}
			retN, retErr = io.ReadFull(dr, p)
			nr += base + int64(retN)
			continue
		}
		cr := &countReader{r: io.LimitReader(dr, e.chunkSize)}
		if err := fr.preRead(e.id, e.chunkOffset, e.chunkSize, e.chunkDigest, cr); err != nil {
			return 0, fmt.Errorf("failed to pre read: %w", err)
		}
		nr += cr.n
	}
	if !found {
		return 0, fmt.Errorf("fileReader.ReadAt: target entry not found")
	}
	return retN, retErr
}

// TODO: share it with memory pkg
func attrFromTOCEntry(src *estargz.TOCEntry, dst *metadata.Attr) *metadata.Attr {
	dst.Size = src.Size
	dst.ModTime, _ = time.Parse(time.RFC3339, src.ModTime3339)
	dst.LinkName = src.LinkName
	dst.Mode = src.Stat().Mode()
	dst.UID = src.UID
	dst.GID = src.GID
	dst.DevMajor = src.DevMajor
	dst.DevMinor = src.DevMinor
	dst.Xattrs = src.Xattrs
	dst.NumLink = src.NumLink
	return dst
}

func getIDByName(md map[uint32]*metadataEntry, name string, rootID uint32) (uint32, error) {
	name = cleanEntryName(name)
	if name == "" {
		return rootID, nil
	}
	dir, base := filepath.Split(name)
	pid, err := getIDByName(md, dir, rootID)
	if err != nil {
		return 0, err
	}
	if md[pid] == nil {
		return 0, fmt.Errorf("not found metadata of %d", pid)
	}
	if md[pid].children == nil {
		return 0, fmt.Errorf("not found children of %q", pid)
	}
	c, ok := md[pid].children[base]
	if !ok {
		return 0, fmt.Errorf("not found child %q in %d", base, pid)
	}
	return c.id, nil
}

func setChild(md map[uint32]*metadataEntry, pb *bolt.Bucket, pid uint32, base string, id uint32, isDir bool) error {
	if md[pid] == nil {
		md[pid] = &metadataEntry{}
	}
	if md[pid].children == nil {
		md[pid].children = make(map[string]childEntry)
	}
	md[pid].children[base] = childEntry{base, id}
	if isDir {
		numLink, _ := binary.Varint(pb.Get(bucketKeyNumLink))
		if err := putInt(pb, bucketKeyNumLink, numLink+1); err != nil {
			return fmt.Errorf("cannot add numlink for children: %w", err)
		}
	}
	return nil
}

func parentDir(p string) string {
	dir, _ := path.Split(p)
	return strings.TrimSuffix(dir, "/")
}

func cleanEntryName(name string) string {
	// Use path.Clean to consistently deal with path separators across platforms.
	return strings.TrimPrefix(path.Clean("/"+name), "/")
}

func resetEnt(ent *estargz.TOCEntry) {
	ent.Name = ""
	ent.Type = ""
	ent.Size = 0
	ent.ModTime3339 = ""
	ent.LinkName = ""
	ent.Mode = 0
	ent.UID = 0
	ent.GID = 0
	ent.Uname = ""
	ent.Gname = ""
	ent.Offset = 0
	ent.DevMajor = 0
	ent.DevMinor = 0
	ent.NumLink = 0
	ent.Xattrs = nil
	ent.Digest = ""
	ent.ChunkOffset = 0
	ent.ChunkSize = 0
	ent.ChunkDigest = ""
	ent.InnerOffset = 0
}

func positive(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

func (r *reader) NumOfNodes() (i int, _ error) {
	if err := r.view(func(tx *bolt.Tx) error {
		nodes, err := getNodes(tx, r.fsID)
		if err != nil {
			return err
		}
		return nodes.ForEach(func(k, v []byte) error {
			b := nodes.Bucket(k)
			if b == nil {
				return fmt.Errorf("entry bucket for %q not found", string(k))
			}
			var attr metadata.Attr
			if err := readAttr(b, &attr); err != nil {
				return err
			}
			i++
			return nil
		})
	}); err != nil {
		return 0, err
	}
	return
}

func (r *reader) NumOfChunks(id uint32) (i int, _ error) {
	if err := r.view(func(tx *bolt.Tx) error {
		metadataEntries, err := getMetadata(tx, r.fsID)
		if err != nil {
			return err
		}
		md, err := getMetadataBucketByID(metadataEntries, id)
		if err != nil {
			return err
		}
		nodes, err := getNodes(tx, r.fsID)
		if err != nil {
			return err
		}
		b, err := getNodeBucketByID(nodes, id)
		if err != nil {
			return err
		}
		size, _ := binary.Varint(b.Get(bucketKeySize))
		chunks, err := readChunks(md, size)
		if err != nil {
			return err
		}
		i = len(chunks)
		return nil
	}); err != nil {
		return 0, err
	}
	return
}

type countReader struct {
	r io.Reader
	n int64
}

func (cr *countReader) Read(p []byte) (n int, err error) {
	n, err = cr.r.Read(p)
	cr.n += int64(n)
	return
}
