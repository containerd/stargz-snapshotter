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
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	runc "github.com/containerd/go-runc"
	"github.com/docker/docker/oci"
	"github.com/moby/buildkit/identity"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runc/libcontainer/user"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// TODO: Enable to specify volumes
func Run(bundle string, config v1.Image, period int, opts ...Option) error {
	opt := options{}
	for _, o := range opts {
		o(&opt)
	}

	spec, err := conf2spec(config.Config, GetRootfsPathUnder(bundle), opt)
	if err != nil {
		return fmt.Errorf("failed to convert config to spec: %v", err)
	}
	sf, err := os.Create(filepath.Join(bundle, "config.json"))
	if err = json.NewEncoder(sf).Encode(spec); err != nil {
		return fmt.Errorf("failed to parse user: %v", err)
	}

	// run the container
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		err = runContainer(ctx, bundle)
		close(done)
	}()

	sc := make(chan os.Signal, 1)
	signal.Notify(sc,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	defer signal.Stop(sc)

	select {
	case <-sc:
		fmt.Printf("Signal detected.")
		cancel()
	case <-time.After(time.Duration(period) * time.Second):
		cancel()
	case <-done:
	}

	if err != nil {
		return fmt.Errorf("failed to run containers: %v", err)
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
	if username == "" {
		username = "root"
	}
	if opt.user != "" {
		username = opt.user
	}
	userinfo, err := getUser(username, rootfs)
	if err != nil {
		// fall back to default
		userinfo = specs.User{
			UID: 0,
			GID: 0,
		}
	}
	s.Process.User = userinfo

	// Env
	s.Process.Env = append(config.Env, opt.envs...)

	// WorkingDir
	s.Process.Cwd = config.WorkingDir
	if s.Process.Cwd == "" {
		s.Process.Cwd = "/"
	}
	if opt.workingDir != "" {
		s.Process.Cwd = opt.workingDir
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
	// TODO: Use user-specified signal.
	runtime := &runc.Runc{
		Log:          filepath.Join(bundle, "runc-log.json"),
		LogFormat:    runc.JSON,
		PdeathSignal: syscall.SIGKILL, // this can still leak the process
		// Setpgid:      true,
	}

	id := identity.NewID()
	runCtx, cancelRun := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ctx.Done():
				killCtx, timeout := context.WithTimeout(context.Background(), 7*time.Second)
				if err := runtime.Kill(killCtx, id, int(syscall.SIGKILL), nil); err != nil {
					fmt.Printf("failed to kill runc %s: %+v\n", id, err)
					select {
					case <-killCtx.Done():
						timeout()
						cancelRun()
						return
					default:
					}
				}
				timeout()
				select {
				case <-time.After(50 * time.Millisecond):
				case <-done:
					return
				}
			case <-done:
				return
			}
		}
	}()

	stdio, err := runc.NewSTDIO()
	if err != nil {
		return err
	}
	fmt.Printf("> running container %s\n", id)
	runtime.Run(runCtx, id, bundle, &runc.CreateOpts{
		IO: stdio,
	})
	close(done)
	fmt.Printf("DONE\n")

	return nil
}

// Copyright 2013-2018 Docker, Inc.
// Modified version of getUser function to make it work with any rootfs.
// https://github.com/moby/moby/blob/ad1b781e44fa1e44b9e654e5078929aec56aed66/daemon/oci_linux.go
func getUser(username string, rootfsPath string) (specs.User, error) {
	passwdPath, err := user.GetPasswdPath()
	if err != nil {
		return specs.User{}, err
	}
	groupPath, err := user.GetGroupPath()
	if err != nil {
		return specs.User{}, err
	}
	passwdFile, err := os.Open(filepath.Join(rootfsPath, passwdPath))
	if err == nil {
		defer passwdFile.Close()
	}
	groupFile, err := os.Open(filepath.Join(rootfsPath, groupPath))
	if err == nil {
		defer groupFile.Close()
	}

	execUser, err := user.GetExecUser(username, nil, passwdFile, groupFile)
	if err != nil {
		return specs.User{}, err
	}

	// todo: fix this double read by a change to libcontainer/user pkg
	groupFile, err = os.Open(filepath.Join(rootfsPath, groupPath))
	if err == nil {
		defer groupFile.Close()
	}
	uid := uint32(execUser.Uid)
	gid := uint32(execUser.Gid)
	var additionalGids []uint32
	for _, g := range execUser.Sgids {
		additionalGids = append(additionalGids, uint32(g))
	}

	return specs.User{
		UID:            uid,
		GID:            gid,
		AdditionalGids: additionalGids,
		Username:       username,
	}, nil
}
