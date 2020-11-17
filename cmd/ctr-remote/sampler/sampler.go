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

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/pkg/netns"
	gocni "github.com/containerd/go-cni"
	runc "github.com/containerd/go-runc"
	"github.com/docker/docker/oci"
	"github.com/hashicorp/go-multierror"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runc/libcontainer/user"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/rs/xid"
)

func Run(ctx context.Context, bundle string, config v1.Image, opts ...Option) error {
	opt := options{}
	for _, o := range opts {
		o(&opt)
	}

	spec, done, err := conf2spec(config.Config, GetRootfsPathUnder(bundle), opt)
	if err != nil {
		return errors.Wrap(err, "failed to convert config to spec")
	}
	defer func() {
		if err := done(); err != nil {
			log.G(ctx).WithError(err).Warnf("failed to cleanup")
			return
		}
		log.G(ctx).Infof("cleaned up successfully")
	}()
	sf, err := os.Create(filepath.Join(bundle, "config.json"))
	if err != nil {
		return errors.Wrap(err, "failed to create config.json in the bundle")
	}
	defer sf.Close()
	if err = json.NewEncoder(sf).Encode(spec); err != nil {
		return errors.Wrap(err, "failed to parse user")
	}

	// run the container
	if err := runContainer(ctx, bundle); err != nil {
		return errors.Wrap(err, "failed to run containers")
	}

	return nil
}

func GetRootfsPathUnder(bundle string) string {
	return filepath.Join(bundle, "rootfs")
}

func conf2spec(config v1.ImageConfig, rootfs string, opt options) (spec specs.Spec, done func() error, rErr error) {
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

	s := oci.DefaultSpec()
	s.Root = &specs.Root{
		Path: rootfs,
	}

	// Terminal
	if opt.terminal {
		s.Process.Terminal = true
	}

	// User
	username := config.User
	if opt.user != "" {
		username = opt.user
	}
	if username != "" {
		// Username is specified, we need to resolve the uid and gid
		passwdPath, err := user.GetPasswdPath()
		if err != nil {
			rErr = errors.Wrap(err, "failed to get passwd file path")
			return
		}
		groupPath, err := user.GetGroupPath()
		if err != nil {
			rErr = errors.Wrap(err, "failed to get group file path")
			return
		}
		execUser, err := user.GetExecUserPath(username, nil,
			filepath.Join(rootfs, passwdPath), filepath.Join(rootfs, groupPath))
		if err != nil {
			rErr = errors.Wrapf(err, "failed to resolve username %q", username)
			return
		}
		s.Process.User.UID = uint32(execUser.Uid)
		s.Process.User.GID = uint32(execUser.Gid)
		for _, g := range execUser.Sgids {
			s.Process.User.AdditionalGids = append(s.Process.User.AdditionalGids, uint32(g))
		}
	}

	// Env
	s.Process.Env = append(config.Env, opt.envs...)

	// WorkingDir
	s.Process.Cwd = config.WorkingDir
	if opt.workingDir != "" {
		s.Process.Cwd = opt.workingDir
	}
	if s.Process.Cwd == "" {
		s.Process.Cwd = "/"
	}

	// Entrypoint, Cmd
	entrypoint := config.Entrypoint
	if len(opt.entrypoint) != 0 {
		entrypoint = opt.entrypoint
	}
	args := config.Cmd
	if len(opt.args) != 0 {
		args = opt.args
	}
	s.Process.Args = append(entrypoint, args...)

	// Generate /etc/hosts and /etc/resolv.conf
	resolvDir, err := ioutil.TempDir("", "tmpetc")
	if err != nil {
		rErr = errors.Wrap(err, "failed to prepare tmp dir")
		return
	}
	cleanups = append(cleanups, func() error { return os.RemoveAll(resolvDir) })
	var (
		etcHostsPath      = filepath.Join(resolvDir, "hosts")
		etcResolvConfPath = filepath.Join(resolvDir, "resolv.conf")
		buf               = new(bytes.Buffer)
	)
	for _, n := range opt.dnsNameservers {
		if _, err := fmt.Fprintf(buf, "nameserver %s\n", n); err != nil {
			rErr = errors.Wrap(err, "failed to prepare nameserver of /etc/resolv.conf")
			return
		}
	}
	if len(opt.dnsSearchDomains) > 0 {
		_, err := fmt.Fprintf(buf, "search %s\n", strings.Join(opt.dnsSearchDomains, " "))
		if err != nil {
			rErr = errors.Wrap(err, "failed to prepare search contents of /etc/resolv.conf")
			return
		}
	}
	if len(opt.dnsOptions) > 0 {
		_, err := fmt.Fprintf(buf, "options %s\n", strings.Join(opt.dnsOptions, " "))
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
	for _, h := range opt.extraHosts {
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
	s.Mounts = append(
		[]specs.Mount{
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
		}, s.Mounts...)

	// Mounts (syntax is compatible to ctr command)
	// e.g.) "type=foo,source=/path,destination=/target,options=rbind:rw"
	for _, m := range opt.mounts {
		r := csv.NewReader(strings.NewReader(m))
		fields, err := r.Read()
		if err != nil {
			rErr = errors.Wrap(err, "failed to parse mounts config")
			return
		}
		mc := specs.Mount{}
		for _, field := range fields {
			v := strings.Split(field, "=")
			if len(v) != 2 {
				rErr = fmt.Errorf("invalid (non key=val) mount spec")
				return
			}
			key, val := v[0], v[1]
			switch key {
			case "type":
				mc.Type = val
			case "source", "src":
				mc.Source = val
			case "destination", "dst":
				mc.Destination = val
			case "options":
				mc.Options = strings.Split(val, ":")
			default:
				rErr = fmt.Errorf("mount option %q not supported", key)
				return
			}
		}
		s.Mounts = append(s.Mounts, mc)
	}

	// CNI-based networking (if enabled).
	if opt.cni {
		// Create a new network namespace for configuring it with CNI plugins
		ns, err := netns.NewNetNS()
		if err != nil {
			rErr = errors.Wrapf(err, "failed to prepare netns")
			return
		}
		cleanups = append(cleanups, ns.Remove)

		// Configure the namespace with CNI plugins
		var cniopts []gocni.CNIOpt
		if cdir := opt.cniPluginConfDir; cdir != "" {
			cniopts = append(cniopts, gocni.WithPluginConfDir(cdir))
		}
		if pdir := opt.cniPluginDir; pdir != "" {
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
		ctx := context.Background()
		if _, err := network.Setup(ctx, id, ns.GetPath()); err != nil {
			rErr = errors.Wrap(err, "failed to setup netns with CNI plugins")
			return
		}
		cleanups = append(cleanups, func() error {
			return network.Remove(ctx, id, ns.GetPath())
		})

		// Make the container use this network namespace
		for i, e := range s.Linux.Namespaces {
			if e.Type == specs.NetworkNamespace {
				before := s.Linux.Namespaces[:i]
				after := s.Linux.Namespaces[i+1:]
				s.Linux.Namespaces = append(append(before, specs.LinuxNamespace{
					Type: specs.NetworkNamespace,
					Path: ns.GetPath(),
				}), after...)
				break
			}
		}
	}

	return s, done, nil
}

func runContainer(ctx context.Context, bundle string) error {
	runtime := &runc.Runc{
		Log:          filepath.Join(bundle, "runc-log.json"),
		LogFormat:    runc.JSON,
		PdeathSignal: syscall.SIGKILL,
		// Setpgid:      true,         // TODO: do we need this?
	}

	// Run the container
	id := xid.New().String()
	stdio, err := runc.NewSTDIO()
	if err != nil {
		return err
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	done := make(chan struct{})
	go func() {
		defer close(done)
		log.G(ctx).Infof("running container %q", id)
		runtime.Run(runCtx, id, bundle, &runc.CreateOpts{
			IO: stdio,
		})
		log.G(ctx).Infof("container %q stopped", id)
	}()

	// Wait until context is canceled or signal is detected
	log.G(ctx).Debugf("waiting for the termination of container %q", id)
	sc := make(chan os.Signal, 1)
	signal.Notify(sc,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	defer signal.Stop(sc)
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		log.G(ctx).Info("context canceled")
	case <-sc:
		log.G(ctx).Info("signal detected")
	}

	// Kill the container
	for {
		select {
		case <-done:
			// container terminated
			return nil
		default:
			log.G(ctx).Debugf("trying to kill container %q", id)
			killCtx, timeout := context.WithTimeout(context.Background(), 7*time.Second)
			if err := runtime.Kill(killCtx, id, int(syscall.SIGKILL), nil); err != nil {
				log.G(ctx).WithError(err).Warnf("failed to kill container %q", id)
				select {
				case <-killCtx.Done():
					// runc kill seems to hang. we shouldn't retry this anymore.
					timeout()
					return err
				default:
				}
			}
			timeout()
			select {
			case <-done:
				return nil
			case <-time.After(50 * time.Millisecond):
				// retry runc kill
			}
		}
	}
}
