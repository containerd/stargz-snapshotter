/*
   Copyright The containerd Authors.
   Copyright 2019 The Go Authors.

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

package sorter

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"reflect"
	"strings"
	"testing"

	"github.com/ktock/stargz-snapshotter/stargz"
)

func TestSort(t *testing.T) {
	longname1 := longstring(120)
	longname2 := longstring(150)

	tests := []struct {
		name string
		in   []tarent
		log  []string
		want []tarent
	}{
		{
			name: "nolog",
			in: []tarent{
				regfile("foo.txt", "foo"),
				directory("bar/"),
				regfile("bar/baz.txt", "baz"),
				regfile("bar/bar.txt", "bar"),
			},
			want: []tarent{
				regfile("foo.txt", "foo"),
				directory("bar/"),
				regfile("bar/baz.txt", "baz"),
				regfile("bar/bar.txt", "bar"),
			},
		},
		{
			name: "identical",
			in: []tarent{
				regfile("foo.txt", "foo"),
				directory("bar/"),
				regfile("bar/baz.txt", "baz"),
				regfile("bar/bar.txt", "bar"),
				regfile("bar/baa.txt", "baa"),
			},
			log: []string{"foo.txt", "bar/baz.txt"},
			want: []tarent{
				regfile("foo.txt", "foo"),
				directory("bar/"),
				regfile("bar/baz.txt", "baz"),
				landmark(),
				regfile("bar/bar.txt", "bar"),
				regfile("bar/baa.txt", "baa"),
			},
		},
		{
			name: "shuffle_regfile",
			in: []tarent{
				regfile("foo.txt", "foo"),
				regfile("baz.txt", "baz"),
				regfile("bar.txt", "bar"),
				regfile("baa.txt", "baa"),
			},
			log: []string{"baa.txt", "bar.txt", "baz.txt"},
			want: []tarent{
				regfile("baa.txt", "baa"),
				regfile("bar.txt", "bar"),
				regfile("baz.txt", "baz"),
				landmark(),
				regfile("foo.txt", "foo"),
			},
		},
		{
			name: "shuffle_directory",
			in: []tarent{
				regfile("foo.txt", "foo"),
				directory("bar/"),
				regfile("bar/bar.txt", "bar"),
				regfile("bar/baa.txt", "baa"),
				directory("baz/"),
				regfile("baz/baz1.txt", "baz"),
				regfile("baz/baz2.txt", "baz"),
				directory("baz/bazbaz/"),
				regfile("baz/bazbaz/bazbaz_b.txt", "baz"),
				regfile("baz/bazbaz/bazbaz_a.txt", "baz"),
			},
			log: []string{"baz/bazbaz/bazbaz_a.txt", "baz/baz2.txt", "foo.txt"},
			want: []tarent{
				directory("baz/"),
				directory("baz/bazbaz/"),
				regfile("baz/bazbaz/bazbaz_a.txt", "baz"),
				regfile("baz/baz2.txt", "baz"),
				regfile("foo.txt", "foo"),
				landmark(),
				directory("bar/"),
				regfile("bar/bar.txt", "bar"),
				regfile("bar/baa.txt", "baa"),
				regfile("baz/baz1.txt", "baz"),
				regfile("baz/bazbaz/bazbaz_b.txt", "baz"),
			},
		},
		{
			name: "shuffle_link",
			in: []tarent{
				regfile("foo.txt", "foo"),
				regfile("baz.txt", "baz"),
				hardlink("bar.txt", "baz.txt"),
				regfile("baa.txt", "baa"),
			},
			log: []string{"baz.txt"},
			want: []tarent{
				regfile("baz.txt", "baz"),
				landmark(),
				regfile("foo.txt", "foo"),
				hardlink("bar.txt", "baz.txt"),
				regfile("baa.txt", "baa"),
			},
		},
		{
			name: "longname",
			in: []tarent{
				regfile("foo.txt", "foo"),
				regfile(longname1, "test"),
				directory("bar/"),
				regfile("bar/bar.txt", "bar"),
				regfile("bar/baa.txt", "baa"),
				regfile(fmt.Sprintf("bar/%s", longname2), "test2"),
			},
			log: []string{fmt.Sprintf("bar/%s", longname2), longname1},
			want: []tarent{
				directory("bar/"),
				regfile(fmt.Sprintf("bar/%s", longname2), "test2"),
				regfile(longname1, "test"),
				landmark(),
				regfile("foo.txt", "foo"),
				regfile("bar/bar.txt", "bar"),
				regfile("bar/baa.txt", "baa"),
			},
		},
		{
			name: "various_types",
			in: []tarent{
				regfile("foo.txt", "foo"),
				symlink("foo2", "foo.txt"),
				devchar("foochar", 10, 50),
				devblock("fooblock", 15, 20),
				fifo("fifoo"),
			},
			log: []string{"fifoo", "foo2", "foo.txt", "fooblock"},
			want: []tarent{
				fifo("fifoo"),
				symlink("foo2", "foo.txt"),
				regfile("foo.txt", "foo"),
				devblock("fooblock", 15, 20),
				landmark(),
				devchar("foochar", 10, 50),
			},
		},
		{
			name: "existing_landmark",
			in: []tarent{
				regfile("baa.txt", "baa"),
				regfile("bar.txt", "bar"),
				regfile("baz.txt", "baz"),
				landmark(),
				regfile("foo.txt", "foo"),
			},
			log: []string{"foo.txt", "bar.txt"},
			want: []tarent{
				regfile("foo.txt", "foo"),
				regfile("bar.txt", "bar"),
				landmark(),
				regfile("baa.txt", "baa"),
				regfile("baz.txt", "baz"),
			},
		},
		{
			name: "existing_landmark_nolog",
			in: []tarent{
				regfile("baa.txt", "baa"),
				regfile("bar.txt", "bar"),
				regfile("baz.txt", "baz"),
				landmark(),
				regfile("foo.txt", "foo"),
			},
			want: []tarent{
				regfile("baa.txt", "baa"),
				regfile("bar.txt", "bar"),
				regfile("baz.txt", "baz"),
				regfile("foo.txt", "foo"),
			},
		},
		{
			name: "not_existing_file",
			in: []tarent{
				regfile("foo.txt", "foo"),
				regfile("baz.txt", "baz"),
				regfile("bar.txt", "bar"),
				regfile("baa.txt", "baa"),
			},
			log: []string{"baa.txt", "bar.txt", "dummy"},
			want: []tarent{
				regfile("baa.txt", "baa"),
				regfile("bar.txt", "bar"),
				landmark(),
				regfile("foo.txt", "foo"),
				regfile("baz.txt", "baz"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Prepare input tar file
			inTar, cancelIn := buildTar(t, tt.in)
			defer cancelIn()
			inTarData, err := ioutil.ReadAll(inTar)
			if err != nil {
				t.Fatalf("failed to read input tar: %q", err)
			}

			// Prepare wanted tar file
			wtr, cancelWant := buildTar(t, tt.want)
			defer cancelWant()
			wantTarData, err := ioutil.ReadAll(wtr)
			if err != nil {
				t.Fatalf("failed to read want tar: %q", err)
			}
			wantTar := tar.NewReader(bytes.NewReader(wantTarData))

			// Sort tar file
			r, err := Sort(bytes.NewReader(inTarData), tt.log)
			if err != nil {
				t.Fatalf("failed to sort: %q", err)
			}
			gotTar := tar.NewReader(r)

			// Compare all
			for {
				// Fetch and parse next header.
				gotH, wantH, err := next(t, gotTar, wantTar)
				if err != nil {
					if err == io.EOF {
						break
					} else {
						t.Fatalf("Failed to parse tar file: %v", err)
					}
				}

				if !reflect.DeepEqual(gotH, wantH) {
					t.Errorf("different header (got = name:%q,type:%d,size:%d; want = name:%q,type:%d,size:%d)",
						gotH.Name, gotH.Typeflag, gotH.Size, wantH.Name, wantH.Typeflag, wantH.Size)
					return

				}

				got, err := ioutil.ReadAll(gotTar)
				if err != nil {
					t.Fatal("failed to read got tar payload")
				}
				want, err := ioutil.ReadAll(wantTar)
				if err != nil {
					t.Fatal("failed to read want tar payload")
				}
				if !bytes.Equal(got, want) {
					t.Errorf("different payload (got = %q; want = %q)", string(got), string(want))
					return
				}
			}
		})
	}
}

func next(t *testing.T, a *tar.Reader, b *tar.Reader) (ah *tar.Header, bh *tar.Header, err error) {
	eofA, eofB := false, false

	ah, err = a.Next()
	if err != nil {
		if err == io.EOF {
			eofA = true
		} else {
			t.Fatalf("Failed to parse tar file: %q", err)
		}
	}

	bh, err = b.Next()
	if err != nil {
		if err == io.EOF {
			eofB = true
		} else {
			t.Fatalf("Failed to parse tar file: %q", err)
		}
	}

	if eofA != eofB {
		if !eofA {
			t.Logf("a = %q", ah.Name)
		}
		if !eofB {
			t.Logf("b = %q", bh.Name)
		}
		t.Fatalf("got eof %t != %t", eofB, eofA)
	}
	if eofA {
		err = io.EOF
	}

	return
}

func buildTar(t *testing.T, ents []tarent) (r io.Reader, cancel func()) {
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		for _, ent := range ents {
			if err := tw.WriteHeader(ent.header); err != nil {
				t.Errorf("writing header to the input tar: %v", err)
				pw.Close()
				return
			}
			if _, err := tw.Write(ent.contents); err != nil {
				t.Errorf("writing contents to the input tar: %v", err)
				pw.Close()
				return
			}
		}
		if err := tw.Close(); err != nil {
			t.Errorf("closing write of input tar: %v", err)
		}
		pw.Close()
	}()
	return pr, func() { go pr.Close(); go pw.Close() }
}

func longstring(size int) (str string) {
	unit := "long"
	for i := 0; i < size/len(unit)+1; i++ {
		str = fmt.Sprintf("%s%s", str, unit)
	}

	return str[:size]
}

type tarent struct {
	header   *tar.Header
	contents []byte
}

func landmark() tarent {
	return tarent{
		header: &tar.Header{
			Name:     stargz.PrefetchLandmark,
			Typeflag: tar.TypeReg,
			Size:     int64(len([]byte{prefetchLandmarkContents})),
		},
		contents: []byte{prefetchLandmarkContents},
	}
}

func regfile(name string, contents string) tarent {
	return tarent{
		header: &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Mode:     0644,
			Size:     int64(len(contents)),
		},
		contents: []byte(contents),
	}
}

func directory(name string) tarent {
	if !strings.HasSuffix(name, "/") {
		panic(fmt.Sprintf("dir %q hasn't suffix /", name))
	}
	return tarent{
		header: &tar.Header{
			Typeflag: tar.TypeDir,
			Name:     name,
			Mode:     0755,
		},
	}
}

func hardlink(name string, linkname string) tarent {
	return tarent{
		header: &tar.Header{
			Typeflag: tar.TypeLink,
			Name:     name,
			Mode:     0644,
			Linkname: linkname,
		},
	}
}

func symlink(name string, linkname string) tarent {
	return tarent{
		header: &tar.Header{
			Typeflag: tar.TypeSymlink,
			Name:     name,
			Mode:     0644,
			Linkname: linkname,
		},
	}
}

func devchar(name string, major int64, minor int64) tarent {
	return tarent{
		header: &tar.Header{
			Typeflag: tar.TypeChar,
			Name:     name,
			Mode:     0644,
			Devmajor: major,
			Devminor: minor,
		},
	}
}

func devblock(name string, major int64, minor int64) tarent {
	return tarent{
		header: &tar.Header{
			Typeflag: tar.TypeBlock,
			Name:     name,
			Mode:     0644,
			Devmajor: major,
			Devminor: minor,
		},
	}
}

func fifo(name string) tarent {
	return tarent{
		header: &tar.Header{
			Typeflag: tar.TypeFifo,
			Name:     name,
			Mode:     0644,
		},
	}
}
