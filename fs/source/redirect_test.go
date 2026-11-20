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

package source

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestCheckRedirect(t *testing.T) {
	newReq := func(ctx context.Context) *http.Request {
		req, err := http.NewRequestWithContext(ctx, "GET", "http://example.com/", nil)
		if err != nil {
			t.Fatalf("failed to make request: %v", err)
		}
		return req
	}
	via := func(n int) []*http.Request {
		return make([]*http.Request, n)
	}

	marked := newReq(WithNoFollowRedirect(context.Background()))
	if err := CheckRedirect(marked, via(1)); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("marked request: got %v; want http.ErrUseLastResponse", err)
	}

	unmarked := newReq(context.Background())
	for _, n := range []int{1, 9} {
		if err := CheckRedirect(unmarked, via(n)); err != nil {
			t.Errorf("unmarked request after %d redirects: got %v; want nil", n, err)
		}
	}
	if err := CheckRedirect(unmarked, via(10)); err == nil || errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("unmarked request after 10 redirects: got %v; want an error", err)
	}
}
