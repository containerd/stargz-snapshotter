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
	envs             []string
	args             []string
	entrypoint       []string
	user             string
	workingDir       string
	terminal         bool
	waitOnSignal     bool
	mounts           []string
	dnsNameservers   []string
	dnsSearchDomains []string
	dnsOptions       []string
	extraHosts       []string
	cni              bool
	cniPluginConfDir string
	cniPluginDir     string
}

func WithEnvs(envs []string) Option {
	return func(opts *options) {
		opts.envs = envs
	}
}

func WithMounts(mounts []string) Option {
	return func(opts *options) {
		opts.mounts = mounts
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

func WithWaitOnSignal() Option {
	return func(opts *options) {
		opts.waitOnSignal = true
	}
}

func WithDNSNameservers(dnsNameservers []string) Option {
	return func(opts *options) {
		opts.dnsNameservers = dnsNameservers
	}
}

func WithDNSSearchDomains(dnsSearchDomains []string) Option {
	return func(opts *options) {
		opts.dnsSearchDomains = dnsSearchDomains
	}
}

func WithDNSOptions(dnsOptions []string) Option {
	return func(opts *options) {
		opts.dnsOptions = dnsOptions
	}
}

func WithExtraHosts(extraHosts []string) Option {
	return func(opts *options) {
		opts.extraHosts = extraHosts
	}
}

func WithCNI() Option {
	return func(opts *options) {
		opts.cni = true
	}
}

func WithCNIPluginConfDir(cniPluginConfDir string) Option {
	return func(opts *options) {
		opts.cniPluginConfDir = cniPluginConfDir
	}
}

func WithCNIPluginDir(cniPluginDir string) Option {
	return func(opts *options) {
		opts.cniPluginDir = cniPluginDir
	}
}
