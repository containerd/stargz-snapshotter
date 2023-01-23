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

package client

import (
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/mitchellh/go-homedir"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

// Client is an IPFS API client.
type Client struct {
	// Address is URL of IPFS API to connect to.
	Address string

	// Client is http client to use for connecting to IPFS API
	Client *http.Client
}

// New creates a new IPFS API client of the specified address.
func New(ipfsAPIAddress string) *Client {
	return &Client{Address: ipfsAPIAddress, Client: http.DefaultClient}
}

// FileInfo represents the information provided by "/api/v0/files/stat" API of IPFS.
// Please see details at: https://docs.ipfs.tech/reference/kubo/rpc/#api-v0-files-stat
type FileInfo struct {
	Blocks         int    `json:"Blocks"`
	CumulativeSize uint64 `json:"CumulativeSize": "<uint64>"`
	Hash           string `json:"Hash": "<string>"`
	Local          bool   `json:"Local": "<bool>"`
	Size           uint64 `json:"Size"`
	SizeLocal      uint64 `json:"SizeLocal"`
	Type           string `json:"Type"`
	WithLocality   bool   `json:"WithLocality"`
}

// StatCID gets and returns information of the file specified by the cid.
func (c *Client) StatCID(cid string) (info *FileInfo, retErr error) {
	if c.Address == "" {
		return nil, fmt.Errorf("specify IPFS API address")
	}
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	ipfsAPIFilesStat := c.Address + "/api/v0/files/stat"
	req, err := http.NewRequest("POST", ipfsAPIFilesStat, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	q.Add("arg", "/ipfs/"+cid)
	req.URL.RawQuery = q.Encode()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("failed to stat %v; status code: %v", cid, resp.StatusCode)
	}
	var rs FileInfo
	if err := json.NewDecoder(resp.Body).Decode(&rs); err != nil {
		return nil, err
	}
	return &rs, nil
}

// Get get the reader of the data specified by the IPFS path and optionally with
// the offset and length.
func (c *Client) Get(p string, offset *int, length *int) (_ io.ReadCloser, retErr error) {
	if c.Address == "" {
		return nil, fmt.Errorf("specify IPFS API address")
	}
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	ipfsAPICat := c.Address + "/api/v0/cat"
	req, err := http.NewRequest("POST", ipfsAPICat, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	q.Add("arg", p)
	if offset != nil {
		q.Add("offset", fmt.Sprintf("%d", *offset))
	}
	if length != nil {
		q.Add("length", fmt.Sprintf("%d", *length))
	}
	req.URL.RawQuery = q.Encode()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("failed to cat %v; status code: %v", p, resp.StatusCode)
	}
	return resp.Body, nil
}

// Add adds the provided data to IPFS and returns its CID (v1).
func (c *Client) Add(r io.Reader) (cidv1 string, retErr error) {
	if c.Address == "" {
		return "", fmt.Errorf("specify IPFS API address")
	}
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	ipfsAPIAdd := c.Address + "/api/v0/add"
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	contentType := mw.FormDataContentType()
	go func() {
		fw, err := mw.CreateFormFile("file", "file")
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(fw, r); err != nil {
			pw.CloseWithError(err)
			return
		}
		if err := mw.Close(); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.Close()
	}()
	req, err := http.NewRequest("POST", ipfsAPIAdd, pr)
	if err != nil {
		return "", err
	}
	req.Header.Add("Content-Type", contentType)
	q := req.URL.Query()
	q.Add("cid-version", "1")
	q.Add("pin", "true")
	req.URL.RawQuery = q.Encode()
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("failed to add; status code: %v", resp.StatusCode)
	}
	var rs struct {
		Hash string `json:"Hash"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rs); err != nil {
		return "", err
	}
	if rs.Hash == "" {
		return "", fmt.Errorf("got empty hash")
	}
	return rs.Hash, nil
}

// GetIPFSAPIAddress get IPFS API URL from the specified IPFS repository.
// If ipfsPath == "", then it's default is "~/.ipfs".
// This is compatible to IPFS client behaviour: https://github.com/ipfs/go-ipfs-http-client/blob/171fcd55e3b743c38fb9d78a34a3a703ee0b5e89/api.go#L69-L81
func GetIPFSAPIAddress(ipfsPath string, scheme string) (string, error) {
	if ipfsPath == "" {
		ipfsPath = "~/.ipfs"
	}
	baseDir, err := homedir.Expand(ipfsPath)
	if err != nil {
		return "", err
	}
	api, err := os.ReadFile(filepath.Join(baseDir, "api"))
	if err != nil {
		return "", err
	}
	a, err := ma.NewMultiaddr(strings.TrimSpace(string(api)))
	if err != nil {
		return "", err
	}
	_, iurl, err := manet.DialArgs(a)
	if err != nil {
		return "", err
	}
	iurl = scheme + "://" + iurl
	if _, err := url.Parse(iurl); err != nil {
		return "", err
	}
	return iurl, nil
}
