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
	"io"
	"os"
	"testing"

	"github.com/containerd/stargz-snapshotter/fs/layer"
	fsreader "github.com/containerd/stargz-snapshotter/fs/reader"
	"github.com/containerd/stargz-snapshotter/metadata"
	"github.com/containerd/stargz-snapshotter/metadata/testutil"
	bolt "go.etcd.io/bbolt"
)

func TestReader(t *testing.T) {
	testRunner := &testutil.TestRunner{
		TestingT: t,
		Runner: func(testingT testutil.TestingT, name string, run func(t testutil.TestingT)) {
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

	testutil.TestReader(testRunner, newTestableReader)
}

func TestFSReader(t *testing.T) {
	testRunner := &fsreader.TestRunner{
		TestingT: t,
		Runner: func(testingT fsreader.TestingT, name string, run func(t fsreader.TestingT)) {
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

	fsreader.TestSuiteReader(testRunner, newStore)
}

func TestFSLayer(t *testing.T) {
	testRunner := &layer.TestRunner{
		TestingT: t,
		Runner: func(testingT layer.TestingT, name string, run func(t layer.TestingT)) {
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

	layer.TestSuiteLayer(testRunner, newStore)
}

func newTestableReader(sr *io.SectionReader, opts ...metadata.Option) (testutil.TestableReader, error) {
	f, err := os.CreateTemp("", "readertestdb")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	defer os.Remove(f.Name())
	db, err := bolt.Open(f.Name(), 0600, nil)
	if err != nil {
		return nil, err
	}
	r, err := NewReader(db, sr, opts...)
	if err != nil {
		return nil, err
	}
	return &testableReadCloser{
		TestableReader: r.(*reader),
		closeFn: func() error {
			db.Close()
			return os.Remove(f.Name())
		},
	}, nil
}

func newStore(sr *io.SectionReader, opts ...metadata.Option) (metadata.Reader, error) {
	f, err := os.CreateTemp("", "readertestdb")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	db, err := bolt.Open(f.Name(), 0600, nil)
	if err != nil {
		return nil, err
	}
	r, err := NewReader(db, sr, opts...)
	if err != nil {
		return nil, err
	}
	return &readCloser{
		Reader: r,
		closeFn: func() error {
			db.Close()
			return os.Remove(f.Name())
		},
	}, nil
}

type readCloser struct {
	metadata.Reader
	closeFn func() error
}

func (r *readCloser) Close() error {
	r.closeFn()
	return r.Reader.Close()
}

type testableReadCloser struct {
	testutil.TestableReader
	closeFn func() error
}

func (r *testableReadCloser) Close() error {
	r.closeFn()
	return r.TestableReader.Close()
}
