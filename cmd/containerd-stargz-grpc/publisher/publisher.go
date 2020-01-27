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

package publisher

import (
	"context"

	"github.com/containerd/containerd"
	eventsapi "github.com/containerd/containerd/api/services/events/v1"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/events"
	"github.com/containerd/containerd/log"
	"github.com/containerd/typeurl"
)

// New requires ctx to be associated with a containerd namespace.
func New(ctx context.Context, address string) (events.Publisher, error) {
	pub := &publisher{
		ch: make(chan eventsapi.PublishRequest),
	}
	go pub.routine(ctx, address)
	return pub, nil
}

type publisher struct {
	ch chan eventsapi.PublishRequest
}

func (pub *publisher) routine(ctx context.Context, address string) {
	log.G(ctx).Infof("connecting to containerd %s", address)
	client, err := containerd.New(address)
	if err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to connect to containerd %s", address)
		return
	}
	log.G(ctx).Infof("connected to containerd %s", address)
	evc := eventsapi.NewEventsClient(client.Conn())
	for req := range pub.ch {
		if _, err := evc.Publish(ctx, &req); err != nil {
			err = errdefs.FromGRPC(err)
			log.G(ctx).WithError(err).Warnf("could not publish to topic %q", req.Topic)
		}
	}
}

func (pub *publisher) Publish(ctx context.Context, topic string, ev events.Event) error {
	evAny, err := typeurl.MarshalAny(ev)
	if err != nil {
		return err
	}

	if evAny.TypeUrl == "" {
		// https://stackoverflow.com/questions/39712915/proto-messagename-returns-empty-string
		log.G(ctx).Warn("got empty TypeURL, importing incompatible protobuf packages?")
	}
	req := eventsapi.PublishRequest{
		Topic: topic,
		Event: evAny,
	}
	pub.ch <- req
	return nil
}
