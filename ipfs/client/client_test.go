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

// This test should run via "make test-ipfs".

package client

import (
	"bytes"
	"flag"
	"io"
	"testing"
)

var ipfsAPI string

func init() {
	flag.StringVar(&ipfsAPI, "ipfs-api", "", "Address of IPFS API")
}

func TestIPFSClient(t *testing.T) {
	if ipfsAPI == "" {
		t.Log("Specify IPFS API address for IPFS client tests")
		t.Skip()
		return
	}
	t.Logf("IPFS API address: %q", ipfsAPI)
	c := New(ipfsAPI)
	sampleString := "hello world 0123456789"
	d := bytes.NewReader([]byte(sampleString))
	cid, err := c.Add(d)
	if err != nil {
		t.Errorf("failed to add data to IPFS: %v", err)
		return
	}
	checkData(t, c, cid, 0, len(sampleString), sampleString, len(sampleString))
	checkData(t, c, cid, 10, 4, sampleString[10:14], len(sampleString))
}

func checkData(t *testing.T, c *Client, cid string, off, len int, wantData string, allSize int) {
	st, err := c.StatCID(cid)
	if err != nil {
		t.Errorf("failed to stat data from IPFS: %v", err)
		return
	}
	if st.Size != uint64(allSize) {
		t.Errorf("unexpected size got from IPFS %v; wanted %v", st.Size, allSize)
		return
	}
	dGotR, err := c.Get("/ipfs/"+cid, &off, &len)
	if err != nil {
		t.Errorf("failed to get data from IPFS: %v", err)
		return
	}
	dGot, err := io.ReadAll(dGotR)
	if err != nil {
		t.Errorf("failed to read data from IPFS: %v", err)
		return
	}
	if string(dGot) != wantData {
		t.Errorf("unexpected data got from IPFS %q; wanted %q", string(dGot), wantData)
		return
	}
}
