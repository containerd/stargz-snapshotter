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

package main

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/dialer"
	"github.com/containerd/stargz-snapshotter/store/pb"
	grpc "google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	var addr = "/var/lib/stargz-store/store.sock" // default
	if len(os.Args) >= 2 {
		addr = os.Args[1]
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		panic(err)
	}

	backoffConfig := backoff.DefaultConfig
	backoffConfig.MaxDelay = 3 * time.Second
	connParams := grpc.ConnectParams{
		Backoff: backoffConfig,
	}
	gopts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithConnectParams(connParams),
		grpc.WithContextDialer(dialer.ContextDialer),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(defaults.DefaultMaxRecvMsgSize)),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(defaults.DefaultMaxSendMsgSize)),
	}
	conn, err := grpc.NewClient(dialer.DialAddress(addr), gopts...)
	if err != nil {
		panic(err)
	}
	c := pb.NewControllerClient(conn)
	_, err = c.AddCredential(context.Background(), &pb.AddCredentialRequest{
		Data: data,
	})
	if err != nil {
		panic(err)
	}
}
