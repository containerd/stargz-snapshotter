package layer

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

func DataToRawLine(info *RecorderInfo, path string) (b []byte, err error) {
	d := map[string]string{
		"image":       info.ImageTag,
		"layer_sha":   info.LayerSha,
		"layer_index": strconv.Itoa(info.LayerIndex),
		"path":        path,
	}
	return json.Marshal(d)
}

type Message struct {
	info *RecorderInfo
	path string
}

type B10Recorder struct {
	info   *RecorderInfo
	writer *BufferedWriter
}

func (b B10Recorder) Append(path string) {
	fmt.Println("B10 New recorder")
	b.writer.Write(b.info, path)
}

type RecorderInfo struct {
	LayerSha   string
	LayerIndex int
	ImageTag   string
}

func ParseLabels(labels map[string]string, layerDigest string) (r *RecorderInfo, err error) {
	r = &RecorderInfo{}
	if labels == nil {
		return r, fmt.Errorf("labels is nil")
	}
	var ok bool
	r.LayerSha = layerDigest
	r.ImageTag, ok = labels["containerd.io/snapshot/remote/stargz.reference"]
	if !ok {
		r.ImageTag, ok = labels["containerd.io/snapshot/cri.image-ref"]
		if !ok {
			return r, fmt.Errorf("containerd.io/snapshot/remote/stargz.reference not found")
		}
	}
	rawLayers, ok := labels["containerd.io/snapshot/remote/stargz.layers"]
	if !ok {
		rawLayers, ok = labels["containerd.io/snapshot/cri.image-layers"]
		if !ok {
			return r, fmt.Errorf("containerd.io/snapshot/remote/stargz.layers not found")
		}

	}
	layers := strings.Split(rawLayers, ",")
	r.LayerIndex = -1
	for i, layer := range layers {
		if layer == r.LayerSha {
			r.LayerIndex = i
			break
		}
	}
	if r.LayerIndex == -1 {
		return nil, fmt.Errorf("layer %s not found in layers %s", r.LayerSha, rawLayers)
	}
	return
}

var b10Writer *BufferedWriter

func NewB10Recorder(labels map[string]string, layerDigest string) (*B10Recorder, error) {
	// Create a singleton with a sync.Once to avoid concurrent creation of the same object.
	fmt.Println("B10 New recorder")
	sync.OnceFunc(func() {
		var err error
		b10Writer, err = NewBufferedWriter("/var/log/stargz-records.log", 1024*1024, 5*time.Second, 10000)
		if err != nil {
			panic(err)
		}
	})()
	info, err := ParseLabels(labels, layerDigest)
	if err != nil {
		fmt.Println("b10 recorder labels test", labels)
		return nil, err
	}
	fmt.Println("Recorder info", info)
	return &B10Recorder{
		writer: b10Writer,
		info:   info,
	}, nil
}
