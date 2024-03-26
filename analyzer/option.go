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

package analyzer

import (
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/oci"
)

type analyzerOpts struct {
	period       time.Duration
	waitOnSignal bool
	snapshotter  string
	specOpts     SpecOpts
	terminal     bool
	stdin        bool
	waitLineOut  string
}

// Option is runtime configuration of analyzer container
type Option func(opts *analyzerOpts)

// SpecOpts returns runtime configuration based on the passed image and rootfs path.
type SpecOpts func(image containerd.Image, rootfs string) (opts []oci.SpecOpts, done func() error, err error)

// WithSpecOpts is the runtime configuration
func WithSpecOpts(specOpts SpecOpts) Option {
	return func(opts *analyzerOpts) {
		opts.specOpts = specOpts
	}
}

// WithTerminal enable terminal for the container. This must be specified with WithStdin().
func WithTerminal() Option {
	return func(opts *analyzerOpts) {
		opts.terminal = true
	}
}

// WithStdin attaches stdin to the container
func WithStdin() Option {
	return func(opts *analyzerOpts) {
		opts.stdin = true
	}
}

// WithPeriod is the period to run runtime
func WithPeriod(period time.Duration) Option {
	return func(opts *analyzerOpts) {
		opts.period = period
	}
}

// WithWaitOnSignal disables timeout
func WithWaitOnSignal() Option {
	return func(opts *analyzerOpts) {
		opts.waitOnSignal = true
	}
}

// WithSnapshotter is the snapshotter to use
func WithSnapshotter(snapshotter string) Option {
	return func(opts *analyzerOpts) {
		opts.snapshotter = snapshotter
	}
}

// WithWaitLineOut specifies a substring of a stdout line to be waited.
// When this line is detected, the container will be killed.
func WithWaitLineOut(s string) Option {
	return func(opts *analyzerOpts) {
		opts.waitLineOut = s
	}
}
