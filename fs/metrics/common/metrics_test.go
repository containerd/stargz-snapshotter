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

package commonmetrics

import (
	"fmt"
	"syscall"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestErrnoLabel(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "a full cache volume is reported as such",
			err:  fmt.Errorf("failed to commit cache: %w", syscall.ENOSPC),
			want: "ENOSPC",
		},
		{
			name: "an unwrapped errno",
			err:  syscall.EIO,
			want: "EIO",
		},
		{
			name: "an errno outside the reported set stays low cardinality",
			err:  syscall.Errno(0x7fff),
			want: "other",
		},
		{
			name: "an error carrying no errno",
			err:  fmt.Errorf("unexpected status code 503"),
			want: "none",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ErrnoLabel(tt.err); got != tt.want {
				t.Errorf("got %q; want %q", got, tt.want)
			}
		})
	}
}

func TestIncBlobFetchError(t *testing.T) {
	// A collector may be registered with more than one registry, so this
	// gathers from a private one rather than the default.
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(blobFetchErrors); err != nil {
		t.Fatalf("failed to register: %v", err)
	}

	IncBlobFetchError(nil) // must not be counted
	IncBlobFetchError(fmt.Errorf("writing to cache: %w", syscall.ENOSPC))
	IncBlobFetchError(syscall.ENOSPC)
	IncBlobFetchError(fmt.Errorf("unexpected status code 503"))

	want := map[string]float64{"ENOSPC": 2, "none": 1}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("failed to gather: %v", err)
	}
	got := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "stargz_fs_blob_fetch_errors_total" {
			t.Errorf("unexpected metric %q", f.GetName())
			continue
		}
		for _, m := range f.GetMetric() {
			labels := m.GetLabel()
			if len(labels) != 1 || labels[0].GetName() != "errno" {
				t.Fatalf("got labels %v; want a single errno label", labels)
			}
			got[labels[0].GetValue()] = m.GetCounter().GetValue()
		}
	}

	if len(got) != len(want) {
		t.Fatalf("got %v; want %v", got, want)
	}
	for errno, n := range want {
		if got[errno] != n {
			t.Errorf("errno=%s: got %v; want %v", errno, got[errno], n)
		}
	}
}
