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
	"time"

	"github.com/containerd/log"
	"github.com/moby/sys/mountinfo"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/grpc"

	pb "github.com/containerd/stargz-snapshotter/fusemanager/api"
	"github.com/containerd/stargz-snapshotter/service"
	"github.com/containerd/stargz-snapshotter/snapshot"
)

const (
	FuseManagerNotReady = iota
	FuseManagerWaitInit
	FuseManagerReady
)

type Config struct {
	Config                     service.Config
	IPFS                       bool   `toml:"ipfs" json:"ipfs"`
	MetadataStore              string `toml:"metadata_store" default:"memory" json:"metadata_store"`
	DefaultImageServiceAddress string `json:"default_image_service_address"`
}

type ConfigContext struct {
	Ctx        context.Context
	Config     *Config
	RootDir    string
	Server     *grpc.Server
	OpenBoltDB func(string) (*bolt.DB, error)
	Address    string
	CRIServer  *grpc.Server
}

var (
	configFuncs []ConfigFunc
	configMu    sync.Mutex
)

type ConfigFunc func(cc *ConfigContext) ([]service.Option, error)

func RegisterConfigFunc(f ConfigFunc) {
	configMu.Lock()
	defer configMu.Unlock()
	configFuncs = append(configFuncs, f)
}

// Opens bolt DB with avoiding opening the same DB multiple times
type dbOpener struct {
	mu      sync.Mutex
	handles map[string]*bolt.DB
}

func (o *dbOpener) openBoltDB(p string) (*bolt.DB, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if db, ok := o.handles[p]; ok && db != nil {
		// we opened it before. avoid trying to open this again.
		return db, nil
	}

	db, err := bolt.Open(p, 0600, &bolt.Options{
		NoFreelistSync:  true,
		InitialMmapSize: 64 * 1024 * 1024,
		FreelistType:    bolt.FreelistMapType,
	})
	if err != nil {
		return nil, err
	}
	if o.handles == nil {
		o.handles = make(map[string]*bolt.DB)
	}
	o.handles[p] = db
	return db, nil
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

	dbOpener *dbOpener

	serverAddr string

	curCRIServer *grpc.Server
}

func NewFuseManager(ctx context.Context, listener net.Listener, server *grpc.Server, fuseStoreAddr string, serverAddr string) (*Server, error) {
	if err := os.MkdirAll(filepath.Dir(fuseStoreAddr), 0700); err != nil {
		return nil, fmt.Errorf("failed to create directory %q: %w", filepath.Dir(fuseStoreAddr), err)
	}

	db, err := bolt.Open(fuseStoreAddr, 0666, &bolt.Options{Timeout: 10 * time.Second, ReadOnly: false})
	if err != nil {
		return nil, fmt.Errorf("failed to configure fusestore: %w", err)
	}

	fm := &Server{
		status:        FuseManagerWaitInit,
		lock:          sync.RWMutex{},
		fsMap:         sync.Map{},
		ms:            db,
		listener:      listener,
		server:        server,
		fuseStoreAddr: fuseStoreAddr,
		dbOpener:      &dbOpener{},
		serverAddr:    serverAddr,
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

	if fm.curCRIServer != nil {
		fm.curCRIServer.Stop()
		fm.curCRIServer = nil
	}

	cc := &ConfigContext{
		Ctx:        ctx,
		Config:     fm.config,
		RootDir:    fm.root,
		Server:     fm.server,
		OpenBoltDB: fm.dbOpener.openBoltDB,
		Address:    fm.serverAddr,
	}

	var opts []service.Option
	for _, configFunc := range configFuncs {
		funcOpts, err := configFunc(cc)
		if err != nil {
			log.G(ctx).WithError(err).Errorf("failed to apply config function")
			return &pb.Response{}, err
		}
		opts = append(opts, funcOpts...)
	}

	fm.curCRIServer = cc.CRIServer

	fs, err := service.NewFileSystem(ctx, fm.root, &fm.config.Config, opts...)
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
		Config:     fm.config.Config,
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

	err := fm.ms.Close()
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
