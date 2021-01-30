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

// Package recorder provides log recorder
package recorder

import (
	"encoding/json"
	"io"
	"sync"
)

type Entry struct {
	Path           string `json:"path"`
	ManifestDigest string `json:"manifestDigest,omitempty"`
	LayerIndex     *int   `json:"layerIndex,omitempty"`
}

func New(w io.Writer) *Recorder {
	return &Recorder{
		enc: json.NewEncoder(w),
	}
}

// Recorder is thread-safe.
type Recorder struct {
	enc *json.Encoder
	mu  sync.Mutex
}

func (ll *Recorder) Record(e *Entry) error {
	ll.mu.Lock()
	defer ll.mu.Unlock()
	return ll.enc.Encode(e)
}
