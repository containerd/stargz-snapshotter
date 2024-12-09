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

package fusemanager

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/log"
	"github.com/moby/sys/mountinfo"
	"github.com/pkg/errors"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/grpc"

	"github.com/containerd/stargz-snapshotter/cmd/containerd-stargz-grpc/fsopts"
	pb "github.com/containerd/stargz-snapshotter/cmd/stargz-fuse-manager/fusemanager/api"
	"github.com/containerd/stargz-snapshotter/service"
	"github.com/containerd/stargz-snapshotter/service/keychain/keychainconfig"
	"github.com/containerd/stargz-snapshotter/snapshot"
)

const (
	FuseManagerNotReady = iota
	FuseManagerWaitInit
	FuseManagerReady
)

type Config struct {
	Config                     *service.Config
	IPFS                       bool
	MetadataStore              string
	DefaultImageServiceAddress string
}

type Server struct {
	pb.UnimplementedStargzFuseManagerServiceServer

	lock   sync.RWMutex
	status int32

	listener net.Listener
	server   *grpc.Server

	// root is the latest root passed from containerd-stargz-grpc
	root string
	// config is the latest config passed from containerd-stargz-grpc
	config *Config
	// fsMap maps mountpoint to its filesystem instance to ensure Mount/Check/Unmount
	// call the proper filesystem
	fsMap sync.Map
	// curFs is filesystem created by latest config
	curFs snapshot.FileSystem
	ms    *bolt.DB

	fuseStoreAddr string
}

func NewFuseManager(ctx context.Context, listener net.Listener, server *grpc.Server, fuseStoreAddr string) (*Server, error) {
	if err := os.MkdirAll(filepath.Dir(fuseStoreAddr), 0700); err != nil {
		return nil, errors.Wrapf(err, "failed to create directory %q", filepath.Dir(fuseStoreAddr))
	}

	db, err := bolt.Open(fuseStoreAddr, 0666, &bolt.Options{Timeout: 10 * time.Second, ReadOnly: false})
	if err != nil {
		return nil, errors.Wrap(err, "failed to configure fusestore")
	}

	fm := &Server{
		status:        FuseManagerWaitInit,
		lock:          sync.RWMutex{},
		fsMap:         sync.Map{},
		ms:            db,
		listener:      listener,
		server:        server,
		fuseStoreAddr: fuseStoreAddr,
	}

	return fm, nil
}

func (fm *Server) Status(ctx context.Context, _ *pb.StatusRequest) (*pb.StatusResponse, error) {
	fm.lock.RLock()
	defer fm.lock.RUnlock()

	return &pb.StatusResponse{
		Status: fm.status,
	}, nil
}

func (fm *Server) Init(ctx context.Context, req *pb.InitRequest) (*pb.Response, error) {
	fm.lock.Lock()
	fm.status = FuseManagerWaitInit
	defer func() {
		fm.status = FuseManagerReady
		fm.lock.Unlock()
	}()

	ctx = log.WithLogger(ctx, log.G(ctx))

	config := &Config{}
	err := json.Unmarshal(req.Config, config)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to get config")
		return &pb.Response{}, err
	}
	fm.root = req.Root
	fm.config = config

	// Configure keychain
	keyChainConfig := keychainconfig.Config{
		EnableKubeKeychain:         config.Config.KubeconfigKeychainConfig.EnableKeychain,
		EnableCRIKeychain:          config.Config.CRIKeychainConfig.EnableKeychain,
		KubeconfigPath:             config.Config.KubeconfigKeychainConfig.KubeconfigPath,
		DefaultImageServiceAddress: config.DefaultImageServiceAddress,
		ImageServicePath:           config.Config.CRIKeychainConfig.ImageServicePath,
	}

	credsFuncs, err := keychainconfig.ConfigKeychain(ctx, fm.server, &keyChainConfig)
	if err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to configure keychain")
	}

	fsConfig := fsopts.Config{
		EnableIpfs:    config.IPFS,
		MetadataStore: config.MetadataStore,
	}
	fsOpts, err := fsopts.ConfigFsOpts(ctx, fm.root, &fsConfig)
	if err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to configure fs config")
	}

	opts := []service.Option{service.WithCredsFuncs(credsFuncs...), service.WithFilesystemOptions(fsOpts...)}

	fs, err := service.NewFileSystem(ctx, fm.root, fm.config.Config, opts...)
	if err != nil {
		return &pb.Response{}, err
	}
	fm.curFs = fs

	err = fm.restoreFuseInfo(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to restore fuse info")
		return &pb.Response{}, err
	}

	return &pb.Response{}, nil
}

func (fm *Server) Mount(ctx context.Context, req *pb.MountRequest) (*pb.Response, error) {
	fm.lock.RLock()
	defer fm.lock.RUnlock()
	if fm.status != FuseManagerReady {
		return &pb.Response{}, fmt.Errorf("fuse manager not ready")
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("mountpoint", req.Mountpoint))

	err := fm.mount(ctx, req.Mountpoint, req.Labels)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to mount stargz")
		return &pb.Response{}, err
	}

	fm.storeFuseInfo(&fuseInfo{
		Root:       fm.root,
		Mountpoint: req.Mountpoint,
		Labels:     req.Labels,
		Config:     *fm.config.Config,
	})

	return &pb.Response{}, nil
}

func (fm *Server) Check(ctx context.Context, req *pb.CheckRequest) (*pb.Response, error) {
	fm.lock.RLock()
	defer fm.lock.RUnlock()
	if fm.status != FuseManagerReady {
		return &pb.Response{}, fmt.Errorf("fuse manager not ready")
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("mountpoint", req.Mountpoint))

	obj, found := fm.fsMap.Load(req.Mountpoint)
	if !found {
		err := fmt.Errorf("failed to find filesystem of mountpoint %s", req.Mountpoint)
		log.G(ctx).WithError(err).Errorf("failed to check filesystem")
		return &pb.Response{}, err
	}

	fs := obj.(snapshot.FileSystem)
	err := fs.Check(ctx, req.Mountpoint, req.Labels)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to check filesystem")
		return &pb.Response{}, err
	}

	return &pb.Response{}, nil
}

func (fm *Server) Unmount(ctx context.Context, req *pb.UnmountRequest) (*pb.Response, error) {
	fm.lock.RLock()
	defer fm.lock.RUnlock()
	if fm.status != FuseManagerReady {
		return &pb.Response{}, fmt.Errorf("fuse manager not ready")
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("mountpoint", req.Mountpoint))

	obj, found := fm.fsMap.Load(req.Mountpoint)
	if !found {
		// check whether already unmounted
		mounts, err := mountinfo.GetMounts(func(info *mountinfo.Info) (skip, stop bool) {
			if info.Mountpoint == req.Mountpoint {
				return false, true
			}
			return true, false
		})
		if err != nil {
			log.G(ctx).WithError(err).Errorf("failed to get mount info")
			return &pb.Response{}, err
		}

		if len(mounts) <= 0 {
			return &pb.Response{}, nil
		}
		err = fmt.Errorf("failed to find filesystem of mountpoint %s", req.Mountpoint)
		log.G(ctx).WithError(err).Errorf("failed to unmount filesystem")
		return &pb.Response{}, err
	}

	fs := obj.(snapshot.FileSystem)
	err := fs.Unmount(ctx, req.Mountpoint)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to unmount filesystem")
		return &pb.Response{}, err
	}

	fm.fsMap.Delete(req.Mountpoint)
	fm.removeFuseInfo(&fuseInfo{
		Mountpoint: req.Mountpoint,
	})

	return &pb.Response{}, nil
}

func (fm *Server) Close(ctx context.Context) error {
	fm.lock.Lock()
	defer fm.lock.Unlock()
	fm.status = FuseManagerNotReady

	err := fm.clearMounts(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to clear mounts")
		return err
	}

	err = fm.ms.Close()
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to close fusestore")
		return err
	}

	if err := os.Remove(fm.fuseStoreAddr); err != nil {
		log.G(ctx).WithError(err).Errorf("failed to remove fusestore file %s", fm.fuseStoreAddr)
		return err
	}

	return nil
}

func (fm *Server) mount(ctx context.Context, mountpoint string, labels map[string]string) error {
	// mountpoint in fsMap means layer is already mounted, skip it
	if _, found := fm.fsMap.Load(mountpoint); found {
		return nil
	}

	err := fm.curFs.Mount(ctx, mountpoint, labels)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to mount stargz")
		return err
	}

	fm.fsMap.Store(mountpoint, fm.curFs)
	return nil
}

func (fm *Server) clearMounts(ctx context.Context) error {
	mountpoints, err := fm.listMountpoints(ctx)
	if err != nil {
		return err
	}

	for _, mp := range mountpoints {
		if err := syscall.Unmount(mp, syscall.MNT_FORCE); err != nil {
			return err
		}
	}

	return nil
}
