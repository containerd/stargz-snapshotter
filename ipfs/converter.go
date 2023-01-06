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

package ipfs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images/converter"
	"github.com/containerd/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Push pushes the provided image ref to IPFS with converting it to IPFS-enabled format.
func Push(ctx context.Context, client *containerd.Client, ref string, layerConvert converter.ConvertFunc, platformMC platforms.MatchComparer) (cidV1 string, _ error) {
	return PushWithIPFSPath(ctx, client, ref, layerConvert, platformMC, nil)
}

func PushWithIPFSPath(ctx context.Context, client *containerd.Client, ref string, layerConvert converter.ConvertFunc, platformMC platforms.MatchComparer, ipfsPath *string) (cidV1 string, _ error) {
	ctx, done, err := client.WithLease(ctx)
	if err != nil {
		return "", err
	}
	defer done(ctx)
	img, err := client.ImageService().Get(ctx, ref)
	if err != nil {
		return "", err
	}
	desc, err := converter.IndexConvertFuncWithHook(layerConvert, true, platformMC, converter.ConvertHooks{
		PostConvertHook: pushBlobHook(ipfsPath),
	})(ctx, client.ContentStore(), img.Target)
	if err != nil {
		return "", err
	}
	root, err := json.Marshal(desc)
	if err != nil {
		return "", err
	}
	errbuf := new(bytes.Buffer)
	cmd := exec.Command("ipfs", "add", "-Q", "--pin=true", "--cid-version=1")
	cmd.Stderr = errbuf
	cmd.Stdin = bytes.NewReader(root)
	if ipfsPath != nil {
		cmd.Env = append(os.Environ(), fmt.Sprintf("IPFS_PATH=%s", *ipfsPath))
	}
	b, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed ipfs command: %s, %w", string(errbuf.Bytes()), err)
	}
	return strings.TrimSuffix(string(b), "\n"), nil
}

func pushBlobHook(ipfsPath *string) converter.ConvertHookFunc {
	return func(ctx context.Context, cs content.Store, desc ocispec.Descriptor, newDesc *ocispec.Descriptor) (*ocispec.Descriptor, error) {
		resultDesc := newDesc
		if resultDesc == nil {
			descCopy := desc
			resultDesc = &descCopy
		}
		ra, err := cs.ReaderAt(ctx, *resultDesc)
		if err != nil {
			return nil, err
		}
		cmd := exec.Command("ipfs", "add", "-Q", "--pin=true", "--cid-version=1")
		cmd.Stdin = content.NewReader(ra)
		if ipfsPath != nil {
			cmd.Env = append(os.Environ(), fmt.Sprintf("IPFS_PATH=%s", *ipfsPath))
		}
		cidv1, err := cmd.Output()
		if err != nil {
			return nil, err
		}
		resultDesc.URLs = []string{"ipfs://" + strings.TrimSuffix(string(cidv1), "\n")}
		return resultDesc, nil
	}
}

func GetCID(desc ocispec.Descriptor) (string, error) {
	for _, u := range desc.URLs {
		if strings.HasPrefix(u, "ipfs://") {
			return u[7:], nil
		}
	}
	return "", fmt.Errorf("no CID is recorded")
}
