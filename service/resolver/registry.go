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

package resolver

import (
	"fmt"
	"net/http"
	"time"

	"github.com/containerd/containerd/reference"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/containerd/stargz-snapshotter/fs/source"
	rhttp "github.com/hashicorp/go-retryablehttp"
)

const defaultRequestTimeoutSec = 30

// Config is config for resolving registries.
type Config struct {
	Host map[string]HostConfig `toml:"host"`
}

type HostConfig struct {
	Mirrors []MirrorConfig `toml:"mirrors"`
}

type MirrorConfig struct {

	// Host is the hostname of the host.
	Host string `toml:"host"`

	// Insecure is true means use http scheme instead of https.
	Insecure bool `toml:"insecure"`

	// RequestTimeoutSec is timeout seconds of each request to the registry.
	// RequestTimeoutSec == 0 indicates the default timeout (defaultRequestTimeoutSec).
	// RequestTimeoutSec < 0 indicates no timeout.
	RequestTimeoutSec int `toml:"request_timeout_sec"`

	// Header are additional headers to send to the server
	Header map[string]interface{} `toml:"header"`
}

type Credential func(string, reference.Spec) (string, string, error)

// RegistryHostsFromConfig creates RegistryHosts (a set of registry configuration) from Config.
func RegistryHostsFromConfig(cfg Config, credsFuncs ...Credential) source.RegistryHosts {
	return func(ref reference.Spec) (hosts []docker.RegistryHost, _ error) {
		host := ref.Hostname()
		for _, h := range append(cfg.Host[host].Mirrors, MirrorConfig{
			Host: host,
		}) {
			client := rhttp.NewClient()
			client.Logger = nil // disable logging every request
			if h.RequestTimeoutSec >= 0 {
				if h.RequestTimeoutSec == 0 {
					client.HTTPClient.Timeout = defaultRequestTimeoutSec * time.Second
				} else {
					client.HTTPClient.Timeout = time.Duration(h.RequestTimeoutSec) * time.Second
				}
			} // h.RequestTimeoutSec < 0 means "no timeout"
			tr := client.StandardClient()
			var header http.Header
			var err error
			if h.Header != nil {
				header = http.Header{}
				for key, ty := range h.Header {
					switch value := ty.(type) {
					case string:
						header[key] = []string{value}
					case []interface{}:
						header[key], err = makeStringSlice(value, nil)
						if err != nil {
							return nil, err
						}
					default:
						return nil, fmt.Errorf("invalid type %v for header %q", ty, key)
					}
				}
			}
			config := docker.RegistryHost{
				Client:       tr,
				Host:         h.Host,
				Scheme:       "https",
				Path:         "/v2",
				Capabilities: docker.HostCapabilityPull | docker.HostCapabilityResolve,
				Authorizer: docker.NewDockerAuthorizer(
					docker.WithAuthClient(tr),
					docker.WithAuthCreds(multiCredsFuncs(ref, credsFuncs...))),
				Header: header,
			}
			if localhost, _ := docker.MatchLocalhost(config.Host); localhost || h.Insecure {
				config.Scheme = "http"
			}
			if config.Host == "docker.io" {
				config.Host = "registry-1.docker.io"
			}
			hosts = append(hosts, config)
		}
		return
	}
}

func multiCredsFuncs(ref reference.Spec, credsFuncs ...Credential) func(string) (string, string, error) {
	return func(host string) (string, string, error) {
		for _, f := range credsFuncs {
			if username, secret, err := f(host, ref); err != nil {
				return "", "", err
			} else if !(username == "" && secret == "") {
				return username, secret, nil
			}
		}
		return "", "", nil
	}
}

// makeStringSlice is a helper func to convert from []interface{} to []string.
// Additionally an optional cb func may be passed to perform string mapping.
// NOTE: Ported from https://github.com/containerd/containerd/blob/v1.6.9/remotes/docker/config/hosts.go#L516-L533
func makeStringSlice(slice []interface{}, cb func(string) string) ([]string, error) {
	out := make([]string, len(slice))
	for i, value := range slice {
		str, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("unable to cast %v to string", value)
		}

		if cb != nil {
			out[i] = cb(str)
		} else {
			out[i] = str
		}
	}
	return out, nil
}
