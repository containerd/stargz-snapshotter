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
	"fmt"
	"io"
	"net/http"
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
	testPublishAndResolve(t, c, cid)
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

func testPublishAndResolve(t *testing.T, c *Client, cid string) {
	ref := "test/ref:example"

	if err := c.Publish(ref, cid); err != nil {
		t.Errorf("failed to publish CID: %v", err)
		return
	}

	resolvedCID, err := c.Resolve(ref)
	if err != nil {
		t.Errorf("failed to resolve ref: %v", err)
		return
	}

	if resolvedCID != cid {
		t.Errorf("unexpected resolved CID: got %v, want %v", resolvedCID, cid)
	}

	// Clean up the imported key
	if err := c.removeKey(ref); err != nil {
		t.Errorf("failed to remove key: %v", err)
	}
}

// removeKey removes the key associated with the given ref
func (c *Client) removeKey(ref string) error {
	if c.Address == "" {
		return fmt.Errorf("specify IPFS API address")
	}

	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}

	ipfsAPIKeyRemove := c.Address + "/api/v0/key/rm"
	req, err := http.NewRequest("POST", ipfsAPIKeyRemove, nil)
	if err != nil {
		return err
	}

	q := req.URL.Query()
	q.Add("arg", ref)
	req.URL.RawQuery = q.Encode()

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to remove key; status code: %v", resp.StatusCode)
	}

	return nil
}
