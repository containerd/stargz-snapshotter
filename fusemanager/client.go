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

	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/dialer"
	"github.com/containerd/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/containerd/stargz-snapshotter/fusemanager/api"
	"github.com/containerd/stargz-snapshotter/snapshot"
)

type Client struct {
	client pb.StargzFuseManagerServiceClient
}

func NewManagerClient(ctx context.Context, root, socket string, config *Config) (snapshot.FileSystem, error) {
	grpcCli, err := newClient(socket)
	if err != nil {
		return nil, err
	}

	client := &Client{
		client: grpcCli,
	}

	err = client.init(ctx, root, config)
	if err != nil {
		return nil, err
	}

	return client, nil
}

func newClient(socket string) (pb.StargzFuseManagerServiceClient, error) {
	connParams := grpc.ConnectParams{
		Backoff: backoff.DefaultConfig,
	}
	gopts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithConnectParams(connParams),
		grpc.WithContextDialer(dialer.ContextDialer),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(defaults.DefaultMaxRecvMsgSize),
			grpc.MaxCallSendMsgSize(defaults.DefaultMaxSendMsgSize),
		),
	}

	conn, err := grpc.NewClient(fmt.Sprintf("unix://%s", socket), gopts...)
	if err != nil {
		return nil, err
	}

	return pb.NewStargzFuseManagerServiceClient(conn), nil
}

func (cli *Client) init(ctx context.Context, root string, config *Config) error {
	configBytes, err := json.Marshal(config)
	if err != nil {
		return err
	}

	req := &pb.InitRequest{
		Root:   root,
		Config: configBytes,
	}

	_, err = cli.client.Init(ctx, req)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to call Init")
		return err
	}

	return nil
}

func (cli *Client) Mount(ctx context.Context, mountpoint string, labels map[string]string) error {
	req := &pb.MountRequest{
		Mountpoint: mountpoint,
		Labels:     labels,
	}

	_, err := cli.client.Mount(ctx, req)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to call Mount")
		return err
	}

	return nil
}

func (cli *Client) Check(ctx context.Context, mountpoint string, labels map[string]string) error {
	req := &pb.CheckRequest{
		Mountpoint: mountpoint,
		Labels:     labels,
	}

	_, err := cli.client.Check(ctx, req)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to call Check")
		return err
	}

	return nil
}

func (cli *Client) Unmount(ctx context.Context, mountpoint string) error {
	req := &pb.UnmountRequest{
		Mountpoint: mountpoint,
	}

	_, err := cli.client.Unmount(ctx, req)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to call Unmount")
		return err
	}

	return nil
}
