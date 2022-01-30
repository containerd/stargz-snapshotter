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

package errorutil

import (
	"errors"
	"testing"
)

func TestNoError(t *testing.T) {
	if err := Aggregate(nil); err != nil {
		t.Errorf("Aggregate(nil) = %v, wanted nil", err)
	}
	if err := Aggregate([]error{}); err != nil {
		t.Errorf("Aggregate(nil) = %v, wanted nil", err)
	}
}

func TestOneError(t *testing.T) {
	want := errors.New("this is A random Error string")
	if got := Aggregate([]error{want}); got != want {
		t.Errorf("Aggregate() = %v, wanted %v", got, want)
	}
}

func TestMultipleErrors(t *testing.T) {
	want := `3 error(s) occurred:
	* foo
	* bar
	* baz`

	err := Aggregate([]error{
		errors.New("foo"),
		errors.New("bar"),
		errors.New("baz"),
	})
	if err == nil {
		t.Errorf("Aggregate() = nil, wanted %s", want)
	} else if got := err.Error(); got != want {
		t.Errorf("Aggregate() = %s, wanted %s", got, want)
	}
}
