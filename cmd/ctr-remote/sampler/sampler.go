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
	"context"
	"encoding/json"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/containerd/containerd/log"
	runc "github.com/containerd/go-runc"
	"github.com/docker/docker/oci"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runc/libcontainer/user"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/rs/xid"
)

// TODO: Enable to specify volumes
func Run(ctx context.Context, bundle string, config v1.Image, opts ...Option) error {
	opt := options{}
	for _, o := range opts {
		o(&opt)
	}

	spec, err := conf2spec(config.Config, GetRootfsPathUnder(bundle), opt)
	if err != nil {
		return errors.Wrap(err, "failed to convert config to spec")
	}
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

func conf2spec(config v1.ImageConfig, rootfs string, opt options) (specs.Spec, error) {
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
			return specs.Spec{}, errors.Wrapf(err, "failed to get passwd file path")
		}
		groupPath, err := user.GetGroupPath()
		if err != nil {
			return specs.Spec{}, errors.Wrapf(err, "failed to get group file path")
		}
		execUser, err := user.GetExecUserPath(username, nil,
			filepath.Join(rootfs, passwdPath), filepath.Join(rootfs, groupPath))
		if err != nil {
			return specs.Spec{}, errors.Wrapf(err, "failed to resolve username %q", username)
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

	return s, nil
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
