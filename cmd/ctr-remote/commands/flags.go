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

package commands

import (
	"bytes"
	gocontext "context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/oci"
	"github.com/containerd/containerd/pkg/netns"
	gocni "github.com/containerd/go-cni"
	"github.com/hashicorp/go-multierror"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/rs/xid"
	"github.com/urfave/cli"
)

const netnsMountDir = "/var/run/netns"

var samplerFlags = []cli.Flag{
	cli.BoolFlag{
		Name:  "terminal,t",
		Usage: "enable terminal for sample container. must be specified with i option",
	},
	cli.BoolFlag{
		Name:  "i",
		Usage: "attach stdin to the container",
	},
	cli.IntFlag{
		Name:  "period",
		Usage: "time period to monitor access log",
		Value: defaultPeriod,
	},
	cli.StringFlag{
		Name:  "user",
		Usage: "user/group name to override image's default config(user[:group])",
	},
	cli.StringFlag{
		Name:  "cwd",
		Usage: "working dir to override image's default config",
	},
	cli.StringFlag{
		Name:  "args",
		Usage: "command arguments to override image's default config(in JSON array)",
	},
	cli.StringFlag{
		Name:  "entrypoint",
		Usage: "entrypoint to override image's default config(in JSON array)",
	},
	cli.StringSliceFlag{
		Name:  "env",
		Usage: "environment valulable to add or override to the image's default config",
	},
	cli.StringFlag{
		Name:  "env-file",
		Usage: "specify additional container environment variables in a file(i.e. FOO=bar, one per line)",
	},
	cli.StringSliceFlag{
		Name:  "mount",
		Usage: "additional mounts for the container (e.g. type=foo,source=/path,destination=/target,options=bind)",
	},
	cli.StringFlag{
		Name:  "dns-nameservers",
		Usage: "comma-separated nameservers added to the container's /etc/resolv.conf",
		Value: "8.8.8.8",
	},
	cli.StringFlag{
		Name:  "dns-search-domains",
		Usage: "comma-separated search domains added to the container's /etc/resolv.conf",
	},
	cli.StringFlag{
		Name:  "dns-options",
		Usage: "comma-separated options added to the container's /etc/resolv.conf",
	},
	cli.StringFlag{
		Name:  "add-hosts",
		Usage: "comma-separated hosts configuration (host:IP) added to container's /etc/hosts",
	},
	cli.BoolFlag{
		Name:  "cni",
		Usage: "enable CNI-based networking",
	},
	cli.StringFlag{
		Name:  "cni-plugin-conf-dir",
		Usage: "path to the CNI plugins configuration directory",
	},
	cli.StringFlag{
		Name:  "cni-plugin-dir",
		Usage: "path to the CNI plugins binary directory",
	},
}

func getSpecOpts(clicontext *cli.Context) func(image containerd.Image, rootfs string) (opts []oci.SpecOpts, done func() error, rErr error) {
	return func(image containerd.Image, rootfs string) (opts []oci.SpecOpts, done func() error, rErr error) {
		var cleanups []func() error
		done = func() (allErr error) {
			for i := len(cleanups) - 1; i >= 0; i-- {
				if err := cleanups[i](); err != nil {
					allErr = multierror.Append(allErr, err)
				}
			}
			return
		}
		defer func() {
			if rErr != nil {
				if err := done(); err != nil {
					rErr = errors.Wrap(rErr, "failed to cleanup")
				}
			}
		}()

		entrypointOpt, err := withEntrypointArgs(clicontext, image)
		if err != nil {
			rErr = errors.Wrapf(err, "failed to parse entrypoint and arg flags")
			return
		}
		resolverOpt, cleanup, err := withResolveConfig(clicontext)
		if err != nil {
			rErr = errors.Wrapf(err, "failed to parse DNS-related flags")
			return
		}
		cleanups = append(cleanups, cleanup)
		var mounts []runtimespec.Mount
		for _, mount := range clicontext.StringSlice("mount") {
			m, err := parseMountFlag(mount)
			if err != nil {
				rErr = errors.Wrapf(err, "failed to parse mount flag %q", mount)
				return
			}
			mounts = append(mounts, m)
		}
		opts = append(opts,
			oci.WithDefaultSpec(),
			oci.WithDefaultUnixDevices,
			oci.WithRootFSPath(rootfs),
			oci.WithImageConfig(image),
			oci.WithEnv(clicontext.StringSlice("env")),
			oci.WithMounts(mounts),
			resolverOpt,
			entrypointOpt,
		)
		if envFile := clicontext.String("env-file"); envFile != "" {
			opts = append(opts, oci.WithEnvFile(envFile))
		}
		if username := clicontext.String("user"); username != "" {
			opts = append(opts, oci.WithUser(username))
		}
		if cwd := clicontext.String("cwd"); cwd != "" {
			opts = append(opts, oci.WithProcessCwd(cwd))
		}
		if clicontext.Bool("terminal") {
			if !clicontext.Bool("i") {
				rErr = fmt.Errorf("terminal flag must be specified with \"-i\"")
				return
			}
			opts = append(opts, oci.WithTTY)
		}
		if clicontext.Bool("cni") {
			var nOpt oci.SpecOpts
			nOpt, cleanup, err = withCNI(clicontext)
			if err != nil {
				rErr = errors.Wrapf(err, "failed to parse CNI-related flags")
				return
			}
			cleanups = append(cleanups, cleanup)
			opts = append(opts, nOpt)
		}

		return
	}
}

func withEntrypointArgs(clicontext *cli.Context, image containerd.Image) (oci.SpecOpts, error) {
	var eFlag []string
	if eStr := clicontext.String("entrypoint"); eStr != "" {
		if err := json.Unmarshal([]byte(eStr), &eFlag); err != nil {
			return nil, errors.Wrapf(err, "invalid option \"entrypoint\"")
		}
	}
	var aFlag []string
	if aStr := clicontext.String("args"); aStr != "" {
		if err := json.Unmarshal([]byte(aStr), &aFlag); err != nil {
			return nil, errors.Wrapf(err, "invalid option \"args\"")
		}
	}
	return func(ctx gocontext.Context, client oci.Client, container *containers.Container, s *runtimespec.Spec) error {
		configDesc, err := image.Config(ctx)
		if err != nil {
			return err
		}
		var ociimage imagespec.Image
		switch configDesc.MediaType {
		case imagespec.MediaTypeImageConfig, images.MediaTypeDockerSchema2Config:
			p, err := content.ReadBlob(ctx, image.ContentStore(), configDesc)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(p, &ociimage); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown image config media type %s", configDesc.MediaType)
		}
		entrypoint := ociimage.Config.Entrypoint
		if len(eFlag) > 0 {
			entrypoint = eFlag
		}
		args := ociimage.Config.Cmd
		if len(aFlag) > 0 {
			args = aFlag
		}
		return oci.WithProcessArgs(append(entrypoint, args...)...)(ctx, client, container, s)
	}, nil
}

func withCNI(clicontext *cli.Context) (specOpt oci.SpecOpts, done func() error, rErr error) {
	var cleanups []func() error
	done = func() (allErr error) {
		for i := len(cleanups) - 1; i >= 0; i-- {
			if err := cleanups[i](); err != nil {
				allErr = multierror.Append(allErr, err)
			}
		}
		return
	}
	defer func() {
		if rErr != nil {
			if err := done(); err != nil {
				rErr = errors.Wrap(rErr, "failed to cleanup")
			}
		}
	}()

	// Create a new network namespace for configuring it with CNI plugins
	ns, err := netns.NewNetNS(netnsMountDir)
	if err != nil {
		rErr = errors.Wrapf(err, "failed to prepare netns")
		return
	}
	cleanups = append(cleanups, ns.Remove)

	// Configure the namespace with CNI plugins
	var cniopts []gocni.Opt
	if cdir := clicontext.String("cni-plugin-conf-dir"); cdir != "" {
		cniopts = append(cniopts, gocni.WithPluginConfDir(cdir))
	}
	if pdir := clicontext.String("cni-plugin-dir"); pdir != "" {
		cniopts = append(cniopts, gocni.WithPluginDir([]string{pdir}))
	}
	// The first-found configration file will be effective
	// TODO: Should we make the number of reading files configurable?
	cniopts = append(cniopts, gocni.WithDefaultConf)
	network, err := gocni.New(cniopts...)
	if err != nil {
		rErr = errors.Wrap(err, "failed to prepare CNI plugins")
		return
	}
	id := xid.New().String()
	ctx := gocontext.Background()
	if _, err := network.Setup(ctx, id, ns.GetPath()); err != nil {
		rErr = errors.Wrap(err, "failed to setup netns with CNI plugins")
		return
	}
	cleanups = append(cleanups, func() error {
		return network.Remove(ctx, id, ns.GetPath())
	})

	// Make the container use this network namespace
	return oci.WithLinuxNamespace(runtimespec.LinuxNamespace{
		Type: runtimespec.NetworkNamespace,
		Path: ns.GetPath(),
	}), done, nil
}

func withResolveConfig(clicontext *cli.Context) (specOpt oci.SpecOpts, cleanup func() error, rErr error) {
	defer func() {
		if rErr != nil {
			if err := cleanup(); err != nil {
				rErr = errors.Wrap(rErr, "failed to cleanup")
			}
		}
	}()

	extrahosts, nameservers, searches, dnsopts, err := parseResolveFlag(clicontext)
	if err != nil {
		return nil, nil, err
	}

	// Generate /etc/hosts and /etc/resolv.conf
	resolvDir, err := ioutil.TempDir("", "tmpetc")
	if err != nil {
		return nil, nil, err
	}
	cleanup = func() error { return os.RemoveAll(resolvDir) }
	var (
		etcHostsPath      = filepath.Join(resolvDir, "hosts")
		etcResolvConfPath = filepath.Join(resolvDir, "resolv.conf")
		buf               = new(bytes.Buffer)
	)
	for _, n := range nameservers {
		if _, err := fmt.Fprintf(buf, "nameserver %s\n", n); err != nil {
			rErr = errors.Wrap(err, "failed to prepare nameserver of /etc/resolv.conf")
			return
		}
	}

	if len(searches) > 0 {
		_, err := fmt.Fprintf(buf, "search %s\n", strings.Join(searches, " "))
		if err != nil {
			rErr = errors.Wrap(err, "failed to prepare search contents of /etc/resolv.conf")
			return
		}
	}
	if len(dnsopts) > 0 {
		_, err := fmt.Fprintf(buf, "options %s\n", strings.Join(dnsopts, " "))
		if err != nil {
			rErr = errors.Wrap(err, "failed to prepare options contents of /etc/resolv.conf")
			return
		}
	}
	if err := ioutil.WriteFile(etcResolvConfPath, buf.Bytes(), 0644); err != nil {
		rErr = errors.Wrap(err, "failed to write contents to /etc/resolv.conf")
		return
	}
	buf.Reset() // Reusing for /etc/hosts
	for _, h := range []struct {
		host string
		ip   string
	}{
		// Configuration compatible to docker's config
		// https://github.com/moby/libnetwork/blob/535ef365dc1dd82a5135803a58bc6198a3b9aa27/etchosts/etchosts.go#L28-L36
		{"localhost", "127.0.0.1"},
		{"localhost ip6-localhost ip6-loopback", "::1"},
		{"ip6-localnet", "fe00::0"},
		{"ip6-mcastprefix", "ff00::0"},
		{"ip6-allnodes", "ff02::1"},
		{"ip6-allrouters", "ff02::2"},
	} {
		if _, err := fmt.Fprintf(buf, "%s\t%s\n", h.ip, h.host); err != nil {
			rErr = errors.Wrap(err, "failed to write default hosts to /etc/hosts")
			return
		}
	}
	for _, h := range extrahosts {
		parts := strings.SplitN(h, ":", 2) // host:IP
		if len(parts) != 2 {
			rErr = fmt.Errorf("cannot parse %q as extra host; must be \"host:IP\"", h)
			return
		}
		// TODO: Validate them
		if _, err := fmt.Fprintf(buf, "%s\t%s\n", parts[1], parts[0]); err != nil {
			rErr = errors.Wrap(err, "failed to write extra hosts to /etc/hosts")
			return
		}
	}
	if err := ioutil.WriteFile(etcHostsPath, buf.Bytes(), 0644); err != nil {
		rErr = errors.Wrap(err, "failed to write contents to /etc/hosts")
		return
	}

	return oci.WithMounts([]runtimespec.Mount{
		{
			Destination: "/etc/resolv.conf",
			Source:      etcResolvConfPath,
			Options:     []string{"bind"},
		},
		{
			Destination: "/etc/hosts",
			Source:      etcHostsPath,
			Options:     []string{"bind"},
		},
	}), cleanup, nil

}

// parseMountFlag parses a mount string in the form "type=foo,source=/path,destination=/target,options=rbind:rw"
func parseMountFlag(m string) (runtimespec.Mount, error) {
	mount := runtimespec.Mount{}
	r := csv.NewReader(strings.NewReader(m))

	fields, err := r.Read()
	if err != nil {
		return mount, err
	}

	for _, field := range fields {
		v := strings.Split(field, "=")
		if len(v) != 2 {
			return mount, fmt.Errorf("invalid mount specification: expected key=val")
		}

		key := v[0]
		val := v[1]
		switch key {
		case "type":
			mount.Type = val
		case "source", "src":
			mount.Source = val
		case "destination", "dst":
			mount.Destination = val
		case "options":
			mount.Options = strings.Split(val, ":")
		default:
			return mount, fmt.Errorf("mount option %q not supported", key)
		}
	}

	return mount, nil
}

func parseResolveFlag(clicontext *cli.Context) (hosts []string, nameservers []string, searches []string, dnsopts []string, _ error) {
	if nFlag := clicontext.String("dns-nameservers"); nFlag != "" {
		fields, err := csv.NewReader(strings.NewReader(nFlag)).Read()
		if err != nil {
			return nil, nil, nil, nil, err
		}
		nameservers = append(nameservers, fields...)
	}
	if sFlag := clicontext.String("dns-search-domains"); sFlag != "" {
		fields, err := csv.NewReader(strings.NewReader(sFlag)).Read()
		if err != nil {
			return nil, nil, nil, nil, err
		}
		searches = append(searches, fields...)
	}
	if oFlag := clicontext.String("dns-options"); oFlag != "" {
		fields, err := csv.NewReader(strings.NewReader(oFlag)).Read()
		if err != nil {
			return nil, nil, nil, nil, err
		}
		dnsopts = append(dnsopts, fields...)
	}
	if hFlag := clicontext.String("add-hosts"); hFlag != "" {
		fields, err := csv.NewReader(strings.NewReader(hFlag)).Read()
		if err != nil {
			return nil, nil, nil, nil, err
		}
		hosts = append(hosts, fields...)
	}
	return
}
