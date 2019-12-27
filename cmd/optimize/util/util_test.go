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

package util

import (
	"bytes"
	"io"
	"testing"
)

type operations func(*PositionWatcher) error

func TestPositionWatcher(t *testing.T) {
	tests := []struct {
		name    string
		ops     operations
		wantPos int64
	}{
		{
			name: "nop",
			ops: func(pw *PositionWatcher) error {
				return nil
			},
			wantPos: 0,
		},
		{
			name: "read",
			ops: func(pw *PositionWatcher) error {
				size := 5
				if _, err := pw.Read(make([]byte, size)); err != nil {
					return err
				}
				return nil
			},
			wantPos: 5,
		},
		{
			name: "readtwice",
			ops: func(pw *PositionWatcher) error {
				size1, size2 := 5, 3
				if _, err := pw.Read(make([]byte, size1)); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Read(make([]byte, size2)); err != nil {
					if err != io.EOF {
						return err
					}
				}
				return nil
			},
			wantPos: 8,
		},
		{
			name: "seek_start",
			ops: func(pw *PositionWatcher) error {
				size := int64(5)
				if _, err := pw.Seek(size, io.SeekStart); err != nil {
					if err != io.EOF {
						return err
					}
				}
				return nil
			},
			wantPos: 5,
		},
		{
			name: "seek_start_twice",
			ops: func(pw *PositionWatcher) error {
				size1, size2 := int64(5), int64(3)
				if _, err := pw.Seek(size1, io.SeekStart); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Seek(size2, io.SeekStart); err != nil {
					if err != io.EOF {
						return err
					}
				}
				return nil
			},
			wantPos: 3,
		},
		{
			name: "seek_current",
			ops: func(pw *PositionWatcher) error {
				size := int64(5)
				if _, err := pw.Seek(size, io.SeekCurrent); err != nil {
					if err != io.EOF {
						return err
					}
				}
				return nil
			},
			wantPos: 5,
		},
		{
			name: "seek_current_twice",
			ops: func(pw *PositionWatcher) error {
				size1, size2 := int64(5), int64(3)
				if _, err := pw.Seek(size1, io.SeekCurrent); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Seek(size2, io.SeekCurrent); err != nil {
					if err != io.EOF {
						return err
					}
				}
				return nil
			},
			wantPos: 8,
		},
		{
			name: "seek_current_twice_negative",
			ops: func(pw *PositionWatcher) error {
				size1, size2 := int64(5), int64(-3)
				if _, err := pw.Seek(size1, io.SeekCurrent); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Seek(size2, io.SeekCurrent); err != nil {
					if err != io.EOF {
						return err
					}
				}
				return nil
			},
			wantPos: 2,
		},
		{
			name: "mixed",
			ops: func(pw *PositionWatcher) error {
				size1, size2, size3, size4, size5 := int64(5), int64(-3), int64(4), int64(-1), int64(6)
				if _, err := pw.Read(make([]byte, size1)); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Seek(size2, io.SeekCurrent); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Seek(size3, io.SeekStart); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Seek(size4, io.SeekCurrent); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Read(make([]byte, size5)); err != nil {
					if err != io.EOF {
						return err
					}
				}
				return nil
			},
			wantPos: 9,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pw, err := NewPositionWatcher(bytes.NewReader(make([]byte, 100)))
			if err != nil {
				t.Fatalf("failed to make position watcher: %q", err)
			}

			if err := tt.ops(pw); err != nil {
				t.Fatalf("failed to operate position watcher: %q", err)
			}

			gotPos := pw.CurrentPos()
			if tt.wantPos != gotPos {
				t.Errorf("current position: %d; want %d", gotPos, tt.wantPos)
			}
		})
	}

}
