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

package sampler

type Option func(*options)

type options struct {
	envs       []string
	args       []string
	entrypoint []string
	user       string
	workingDir string
	terminal   bool
}

func WithEnvs(envs []string) Option {
	return func(opts *options) {
		opts.envs = envs
	}
}

func WithArgs(args []string) Option {
	return func(opts *options) {
		opts.args = args
	}
}

func WithEntrypoint(entrypoint []string) Option {
	return func(opts *options) {
		opts.entrypoint = entrypoint
	}
}

func WithUser(user string) Option {
	return func(opts *options) {
		opts.user = user
	}
}

func WithWorkingDir(workingDir string) Option {
	return func(opts *options) {
		opts.workingDir = workingDir
	}
}

func WithTerminal() Option {
	return func(opts *options) {
		opts.terminal = true
	}
}
