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
)

type noFollowRedirectKey struct{}

// WithNoFollowRedirect marks ctx so that requests sent with it through a
// client using CheckRedirect return the 3xx response instead of following it.
// fs/remote uses this to capture the blob URL a registry redirects to
// (e.g. a presigned S3 URL from Amazon ECR) so it can be reused for range
// requests.
func WithNoFollowRedirect(ctx context.Context) context.Context {
	return context.WithValue(ctx, noFollowRedirectKey{}, true)
}

// CheckRedirect is an http.Client CheckRedirect policy. Requests whose context
// was marked with WithNoFollowRedirect stop at the first redirect; all other
// requests keep net/http's default behaviour (follow up to 10 redirects), so
// containerd's own fetcher and token auth are unaffected.
func CheckRedirect(req *http.Request, via []*http.Request) error {
	if v, _ := req.Context().Value(noFollowRedirectKey{}).(bool); v {
		return http.ErrUseLastResponse
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return nil
}
