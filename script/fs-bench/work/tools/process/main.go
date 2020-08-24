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
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/client_golang/prometheus"
	commonConfig "github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	promConfig "github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/storage/remote"
)

var (
	config  = flag.String("config", "process.conf", "config file, each line contains a PromQL")
	input   = flag.String("input", "-", "input file, specify - for /dev/stdin")
	output  = flag.String("output", "output", "output dir")
	timefmt = flag.String("timefmt", "incsec", "output time format, "+
		"one of incsec(increasing-seconds), raw(milliseconds-timestamp)")
)

var (
	inputFile  = os.Stdin
	configFile *os.File

	server        *httptest.Server
	remoteStorage *remote.Storage
	engine        *promql.Engine

	queries        = map[string]string{}
	results        = map[string][]promql.Point{}
	storage        = map[string][]prompb.Sample{}
	oldestTs int64 = math.MaxInt64
	newesTs  int64 = -1
)

func main() {
	initme()
	log.Print("inited")

	if err := buildMemRemoteStorage(); err != nil {
		log.Fatalf("build memory remote storage: %s", err)
	}

	if err := setupStorage(); err != nil {
		log.Fatalf("setup remote server: %s", err)
	}

	if err := executePromQL(); err != nil {
		log.Fatalf("execute promql: %s", err)
	}

	if err := outputResults(); err != nil {
		log.Fatalf("output result: %s", err)
	}
}

func initme() {
	flag.Parse()
	var err error

	if err := os.MkdirAll(*output, 0755); err != nil {
		log.Fatalf("make dir %q: %s", *output, err)
	}

	if *input != "-" && *input != "/dev/stdin" {
		inputFile, err = os.OpenFile(*input, os.O_RDONLY|os.O_EXCL, 0444)
		if err != nil {
			log.Fatalf("open %q to read: %s", *input, err)
		}
	}

	configFile, err = os.OpenFile(*config, os.O_RDONLY|os.O_EXCL, 0444)
	if err != nil {
		log.Fatalf("open config %q: %s", *config, err)
	}

	sin := bufio.NewScanner(configFile)
	for sin.Scan() {
		q := sin.Text()
		fields := strings.SplitN(q, ":", 2)
		if len(fields) != 2 {
			log.Printf("invalid config line %q, want format <name>:<promql>", q)
			continue
		}
		name := strings.TrimSpace(fields[0])
		pq := strings.TrimSpace(fields[1])
		_, err := promql.ParseExpr(pq)
		if err != nil {
			log.Fatalf("invalid promql %q: %s", q, err)
		}
		queries[name] = pq
	}
	if err := sin.Err(); err != nil {
		log.Fatalf("read config file %q: %s", *config, err)
	}
}

func buildMemRemoteStorage() error {
	defer log.Printf("built memory storage")

	sin := bufio.NewScanner(inputFile)
	for sin.Scan() {
		ln := sin.Text()
		fields := strings.Split(ln, " ")
		if len(fields) != 3 {
			log.Printf("invalid input line %q, want format '<metricName> <timestamp> <value>'", ln)
			continue
		}
		metricName := fields[0]

		timestamp, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			log.Printf("invalid timestamp %q: %s", ln, err)
			continue
		}
		if timestamp < oldestTs {
			oldestTs = timestamp
		}
		if timestamp > newesTs {
			newesTs = timestamp
		}

		value, err := strconv.ParseFloat(fields[2], 64)
		if err != nil {
			log.Printf("invalid value %q: %s", ln, err)
			continue
		}

		storage[metricName] = append(storage[metricName], prompb.Sample{
			Value:     value,
			Timestamp: timestamp,
		})
	}
	err := sin.Err()
	if err != nil {
		return fmt.Errorf("read input file %q: %w", *input, err)
	}

	sortedStorage := map[string][]prompb.Sample{}
	for metricName, samples := range storage {
		sort.Slice(samples, func(i, j int) bool {
			return samples[i].Timestamp < samples[j].Timestamp
		})
		sortedStorage[metricName] = samples
	}
	storage = sortedStorage
	return err
}

func setupStorage() error {
	server = httptest.NewServer(http.HandlerFunc(remoteReadHandler))
	log.Printf("setup mock server")

	remoteStorage = remote.NewStorage(
		&promLogger{log.New(os.Stdout, "remote", log.LstdFlags)},
		prometheus.DefaultRegisterer,
		func() (int64, error) { return time.Now().Unix() * 1000, nil },
		//func() (int64, error) { return oldestTs, nil },
		"data",
		10*time.Hour,
	)
	u, _ := url.Parse(server.URL)
	remoteCfg := &promConfig.Config{RemoteReadConfigs: []*promConfig.RemoteReadConfig{{
		URL:           &commonConfig.URL{u},
		ReadRecent:    true,
		RemoteTimeout: model.Duration(10 * time.Hour),
	}}}

	if err := remoteStorage.ApplyConfig(remoteCfg); err != nil {
		log.Fatalf("apply config %v: %s", remoteCfg)
	}
	log.Printf("setup storage client")

	engine = promql.NewEngine(promql.EngineOpts{
		MaxConcurrent: 1,
		MaxSamples:    1e6,
		Timeout:       10 * time.Hour,
	})
	log.Printf("setup promql engine")
	return nil
}

func remoteReadHandler(w http.ResponseWriter, r *http.Request) {
	compressed, err := ioutil.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(500)
		w.Write([]byte(fmt.Sprintf("read req body: %s", err)))
	}

	reqBuf, err := snappy.Decode(nil, compressed)
	if err != nil {
		w.WriteHeader(500)
		w.Write([]byte(fmt.Sprintf("decompress req body: %s", err)))
	}

	var req prompb.ReadRequest
	if err := proto.Unmarshal(reqBuf, &req); err != nil {
		w.WriteHeader(500)
		w.Write([]byte(fmt.Sprintf("unmarshal req body: %s", err)))
	}

	rsts := []*prompb.QueryResult{}

	for _, q := range req.Queries {
		var name string
		for _, m := range q.Matchers {
			if m.Name == "__name__" {
				name = m.Value
				break
			}
		}
		if len(name) == 0 {
			log.Printf("got prom remote read with out __name__ label: %+v", q)
			continue
		}
		rsts = append(rsts, queryStorage(name, q.StartTimestampMs, q.EndTimestampMs))
	}

	data, err := proto.Marshal(&prompb.ReadResponse{Results: rsts})
	if err != nil {
		w.WriteHeader(500)
		w.Write([]byte(fmt.Sprintf("marshal rsp body: %s", err)))
	}
	compressed = snappy.Encode(nil, data)

	w.Header().Set("Content-Type", "application/x-protobuf")
	w.Header().Set("Content-Encoding", "snappy")
	w.WriteHeader(200)
	w.Write(compressed)
}

func queryStorage(metricName string, start, end int64) *prompb.QueryResult {
	samples := storage[metricName]
	startI := sort.Search(len(samples), func(i int) bool {
		return samples[i].Timestamp >= start
	})
	endI := sort.Search(len(samples), func(i int) bool {
		return samples[i].Timestamp > end
	})
	return &prompb.QueryResult{Timeseries: []*prompb.TimeSeries{
		{
			Labels: []prompb.Label{
				{
					Name:  "__name__",
					Value: metricName,
				},
			},
			Samples: samples[startI:endI],
		},
	}}
}

func executePromQL() error {
	var (
		i     = 0
		start = time.Unix(oldestTs/1000, oldestTs%1000*1e6)
		end   = time.Unix(newesTs/1000, newesTs%1000*1e6)
	)

	for name, query := range queries {
		q, err := engine.NewRangeQuery(remoteStorage, query, start, end, time.Second)
		if err != nil {
			log.Printf("new range query %s %q: %s", name, query, err)
			continue
		}
		i += 1
		log.Printf("(%d/%d) executing %s %q", i, len(queries), name, query)
		rst := q.Exec(context.Background())
		if rst.Err != nil {
			log.Printf("execute %s %q: %s", name, query, rst.Err)
			continue
		}
		mst, err := rst.Matrix()
		if err != nil {
			log.Printf("not a matrix %s %+v: %s", name, rst, err)
			continue
		}
		if len(mst) != 1 {
			log.Printf("want exact 1 vector, got %d vectors", len(mst))
			continue
		}
		results[name] = mst[0].Points
	}
	return nil
}

func outputResults() error {
	sb := &strings.Builder{}
	for name, pts := range results {
		filename := filepath.Join(*output, name)
		f, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			log.Printf("open %q: %s", filename, err)
			continue
		}
		sb.Reset()
		for i := range pts {
			p := &pts[i]
			sb.WriteString(properTimeStr(p.T) + " " + strconv.FormatFloat(p.V, 'g', -1, 64) + "\n")
		}
		if n, err := f.WriteString(sb.String()); err != nil {
			log.Printf("output result to %q, written %d bytes, err %s", f.Name(), n, err)
			continue
		}
	}
	return nil
}

func properTimeStr(t int64) string {
	if *timefmt == "incsec" {
		return strconv.FormatInt((t-oldestTs)/1000, 10)
	}
	return strconv.FormatInt(t, 10)
}

type promLogger struct {
	*log.Logger
}

func (pl *promLogger) Log(kvs ...interface{}) error {
	if len(kvs)%2 != 0 {
		return errors.New("odd number of parameter to Log(kvs...)")
	}
	sb := &strings.Builder{}
	for i := 0; i < len(kvs); i += 2 {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(fmt.Sprintf("%s=%s", kvs[i], kvs[i+1]))
	}
	log.Println(sb.String())
	return nil
}
