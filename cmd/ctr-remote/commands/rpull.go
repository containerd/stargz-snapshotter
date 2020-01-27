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
	"context"
	"encoding/json"
	"fmt"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cmd/ctr/commands"
	"github.com/containerd/containerd/cmd/ctr/commands/content"
	"github.com/containerd/containerd/events"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/log"
	"github.com/containerd/typeurl"
	stargz "github.com/ktock/stargz-snapshotter/stargz/handler"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/urfave/cli"

	// Register grpc event types
	_ "github.com/ktock/stargz-snapshotter/stargz/proto/events"
)

const (
	remoteSnapshotterName = "stargz"
)

var RpullCommand = cli.Command{
	Name:      "rpull",
	Usage:     "pull an image from a registry levaraging stargz snapshotter",
	ArgsUsage: "[flags] <ref>",
	Description: `Fetch and prepare an image for use in containerd levaraging stargz snapshotter.

After pulling an image, it should be ready to use the same reference in a run
command. 
`,
	Flags: append(commands.RegistryFlags, commands.LabelFlag),
	Action: func(context *cli.Context) error {
		var (
			ref = context.Args().First()
		)
		if ref == "" {
			return fmt.Errorf("please provide an image reference to pull")
		}

		client, ctx, cancel, err := commands.NewClient(context)
		if err != nil {
			return err
		}
		defer cancel()

		ctx, done, err := client.WithLease(ctx)
		if err != nil {
			return err
		}
		defer done(ctx)

		config, err := content.NewFetchConfig(ctx, context)
		if err != nil {
			return err
		}

		eventsCancelCh := make(chan struct{})
		go showEvents(ctx, client, eventsCancelCh)
		defer close(eventsCancelCh)
		if err := pull(ctx, client, ref, config); err != nil {
			return err
		}

		return nil
	},
}

func pull(ctx context.Context, client *containerd.Client, ref string, config *content.FetchConfig) error {
	pCtx := ctx
	h := images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		if desc.MediaType != images.MediaTypeDockerSchema1Manifest {
			fmt.Printf("fetching %v... %v\n", desc.Digest.String()[:15], desc.MediaType)
		}
		return nil, nil
	})

	log.G(pCtx).WithField("image", ref).Debug("fetching")
	labels := commands.LabelArgs(config.Labels)
	if _, err := client.Pull(pCtx, ref, []containerd.RemoteOpt{
		containerd.WithPullLabels(labels),
		containerd.WithResolver(config.Resolver),
		containerd.WithImageHandler(h),
		containerd.WithSchema1Conversion,
		containerd.WithPullUnpack,
		containerd.WithPullSnapshotter(remoteSnapshotterName),
		containerd.WithImageHandlerWrapper(stargz.AppendInfoHandlerWrapper(ref)),
	}...); err != nil {
		return err
	}

	return nil
}

func showEvents(ctx context.Context, client *containerd.Client, cancelCh <-chan struct{}) {
	eventsClient := client.EventService()
	// TODO: filter
	eventsCh, errCh := eventsClient.Subscribe(ctx)
	for {
		var e *events.Envelope
		select {
		case e = <-eventsCh:
		case err := <-errCh:
			log.G(ctx).WithError(err).Warn("error during subscribing events")
		case <-cancelCh:
			return
		}
		if e != nil {
			var out []byte
			if e.Event != nil {
				v, err := typeurl.UnmarshalAny(e.Event)
				if err != nil {
					log.G(ctx).WithError(err).Warn("cannot unmarshal an event from Any")
				}
				out, err = json.Marshal(v)
				if err != nil {
					log.G(ctx).WithError(err).Warn("cannot marshal Any into JSON")
				}
			}
			log.G(ctx).Infof("%s: %s", e.Topic, string(out))
		}
	}
}
