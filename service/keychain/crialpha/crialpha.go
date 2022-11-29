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

package crialpha

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/reference"
	distribution "github.com/containerd/containerd/reference/docker"
	"github.com/containerd/stargz-snapshotter/service/resolver"

	runtime_alpha "github.com/containerd/containerd/third_party/k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// containerd newer than 234bf990dca4e81e89f549448aa6b555286eaa7a is required for this plugin.
// If not, use "github.com/containerd/stargz-snapshotter/service/keychain/crialphaforked" instead.

// NewAlphaCRIKeychain provides creds passed through CRI PullImage API.
// Same as NewCRIKeychain but for CRI v1alpha API.
// Containerd doesn't drop v1alpha API support so our proxy also exposes this API as well.
func NewCRIAlphaKeychain(ctx context.Context, connectCRI func() (runtime_alpha.ImageServiceClient, error)) (resolver.Credential, runtime_alpha.ImageServiceServer) {
	server := &instrumentedAlphaService{config: make(map[string]*runtime_alpha.AuthConfig)}
	go func() {
		log.G(ctx).Debugf("Waiting for CRI service is started...")
		for i := 0; i < 100; i++ {
			client, err := connectCRI()
			if err == nil {
				server.criMu.Lock()
				server.cri = client
				server.criMu.Unlock()
				log.G(ctx).Info("connected to backend CRI service")
				return
			}
			log.G(ctx).WithError(err).Warnf("failed to connect to CRI")
			time.Sleep(10 * time.Second)
		}
		log.G(ctx).Warnf("no connection is available to CRI")
	}()
	return server.credentials, server
}

type instrumentedAlphaService struct {
	cri   runtime_alpha.ImageServiceClient
	criMu sync.Mutex

	config   map[string]*runtime_alpha.AuthConfig
	configMu sync.Mutex
}

func (in *instrumentedAlphaService) credentials(host string, refspec reference.Spec) (string, string, error) {
	if host == "docker.io" || host == "registry-1.docker.io" {
		// Creds of "docker.io" is stored keyed by "https://index.docker.io/v1/".
		host = "index.docker.io"
	}
	in.configMu.Lock()
	defer in.configMu.Unlock()
	if cfg, ok := in.config[refspec.String()]; ok {
		var v1cfg runtime.AuthConfig
		if err := alphaReqToV1Req(cfg, &v1cfg); err != nil {
			return "", "", err
		}
		return resolver.ParseAuth(&v1cfg, host)
	}
	return "", "", nil
}

func (in *instrumentedAlphaService) getCRI() (c runtime_alpha.ImageServiceClient) {
	in.criMu.Lock()
	c = in.cri
	in.criMu.Unlock()
	return
}

func (in *instrumentedAlphaService) ListImages(ctx context.Context, r *runtime_alpha.ListImagesRequest) (res *runtime_alpha.ListImagesResponse, err error) {
	cri := in.getCRI()
	if cri == nil {
		return nil, errors.New("server is not initialized yet")
	}
	return cri.ListImages(ctx, r)
}

func (in *instrumentedAlphaService) ImageStatus(ctx context.Context, r *runtime_alpha.ImageStatusRequest) (res *runtime_alpha.ImageStatusResponse, err error) {
	cri := in.getCRI()
	if cri == nil {
		return nil, errors.New("server is not initialized yet")
	}
	return cri.ImageStatus(ctx, r)
}

func (in *instrumentedAlphaService) PullImage(ctx context.Context, r *runtime_alpha.PullImageRequest) (res *runtime_alpha.PullImageResponse, err error) {
	cri := in.getCRI()
	if cri == nil {
		return nil, errors.New("server is not initialized yet")
	}
	refspec, err := parseReference(r.GetImage().GetImage())
	if err != nil {
		return nil, err
	}
	in.configMu.Lock()
	in.config[refspec.String()] = r.GetAuth()
	in.configMu.Unlock()
	return cri.PullImage(ctx, r)
}

func (in *instrumentedAlphaService) RemoveImage(ctx context.Context, r *runtime_alpha.RemoveImageRequest) (_ *runtime_alpha.RemoveImageResponse, err error) {
	cri := in.getCRI()
	if cri == nil {
		return nil, errors.New("server is not initialized yet")
	}
	refspec, err := parseReference(r.GetImage().GetImage())
	if err != nil {
		return nil, err
	}
	in.configMu.Lock()
	delete(in.config, refspec.String())
	in.configMu.Unlock()
	return cri.RemoveImage(ctx, r)
}

func (in *instrumentedAlphaService) ImageFsInfo(ctx context.Context, r *runtime_alpha.ImageFsInfoRequest) (res *runtime_alpha.ImageFsInfoResponse, err error) {
	cri := in.getCRI()
	if cri == nil {
		return nil, errors.New("server is not initialized yet")
	}
	return cri.ImageFsInfo(ctx, r)
}

// NOTE: Ported from https://github.com/containerd/containerd/blob/792294ce06bfbd1fe07b458bfa066e6ef8b17046/pkg/cri/server/instrumented_service.go#L1704-L1717
func alphaReqToV1Req(
	alphar interface{ Marshal() ([]byte, error) },
	v1r interface{ Unmarshal(_ []byte) error },
) error {
	p, err := alphar.Marshal()
	if err != nil {
		return err
	}

	if err = v1r.Unmarshal(p); err != nil {
		return err
	}
	return nil
}

func parseReference(ref string) (reference.Spec, error) {
	namedRef, err := distribution.ParseDockerRef(ref)
	if err != nil {
		return reference.Spec{}, fmt.Errorf("failed to parse image reference %q: %w", ref, err)
	}
	return reference.Parse(namedRef.String())
}
