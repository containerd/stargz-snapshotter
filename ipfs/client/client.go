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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/stargz-snapshotter/ipfs/ipnskey"
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
	CumulativeSize uint64 `json:"CumulativeSize"`
	Hash           string `json:"Hash"`
	Local          bool   `json:"Local"`
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

// Resolve resolves the IPNS name to its corresponding CID.
func (c *Client) Resolve(ref string) (string, error) {
	if c.Address == "" {
		return "", fmt.Errorf("specify IPFS API address")
	}

	peerID, err := c.importKey(ref)
	if err != nil {
		return "", fmt.Errorf("failed to import key: %w", err)
	}

	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}

	ipfsAPINameResolve := c.Address + "/api/v0/name/resolve"
	req, err := http.NewRequest("POST", ipfsAPINameResolve, nil)
	if err != nil {
		return "", err
	}

	q := req.URL.Query()
	q.Add("arg", "/ipns/"+peerID)
	q.Add("nocache", "true")
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
		return "", fmt.Errorf("failed to resolve name %v; status code: %v", peerID, resp.StatusCode)
	}

	// rs represents the information provided by "/api/v0/name/resolve" API of IPFS.
	// Please see details at: https://docs.ipfs.tech/reference/kubo/rpc/#api-v0-name-resolve
	var rs struct {
		Path string `json:"Path"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rs); err != nil {
		return "", err
	}

	parts := strings.Split(rs.Path, "/")
	if len(parts) < 3 || parts[1] != "ipfs" {
		return "", fmt.Errorf("invalid resolved path format: %s", rs.Path)
	}

	// This is compatible to IPFS behaviour: https://docs.ipfs.tech/concepts/ipns/#ipns-keys
	return parts[2], nil
}

// Publish publishes the given CID to IPNS using the key associated with the given ref.
// Please see details at: https://docs.ipfs.tech/reference/kubo/rpc/#api-v0-name-publish
func (c *Client) Publish(ref string, cid string) error {
	if c.Address == "" {
		return fmt.Errorf("specify IPFS API address")
	}

	_, err := c.importKey(ref)
	if err != nil {
		return fmt.Errorf("failed to import key: %w", err)
	}

	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}

	ipfsAPINamePublish := c.Address + "/api/v0/name/publish"
	req, err := http.NewRequest("POST", ipfsAPINamePublish, nil)
	if err != nil {
		return err
	}

	q := req.URL.Query()
	q.Add("arg", "/ipfs/"+cid)
	q.Add("key", ref)
	q.Add("allow-offline", "true")
	req.URL.RawQuery = q.Encode()

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %v", err)
	}

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("failed to publish; status code: %v, body: %s\n"+
			"Request URL: %s", resp.StatusCode, string(respBody), ipfsAPINamePublish)
	}

	return nil
}

// importKey imports the key pair associated with the given ref into the local IPFS node.
// The ref will be used as the key name in IPFS. If the key already exists, it will return nil.
// Please see details at: https://docs.ipfs.tech/reference/kubo/rpc/#api-v0-key-import
func (c *Client) importKey(ref string) (string, error) {
	if c.Address == "" {
		return "", fmt.Errorf("specify IPFS API address")
	}

	keyID, err := c.getKeyIDFromIPFS(ref)
	if err == nil && keyID != "" {
		return keyID, nil
	}

	keyData, err := ipnskey.GenerateKeyData(ref)
	if err != nil {
		return "", fmt.Errorf("failed to generate key data: %w", err)
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	safeFilename := strings.ReplaceAll(ref, "/", "_")
	safeFilename = strings.ReplaceAll(safeFilename, ":", "_")

	part, err := writer.CreateFormFile("file", safeFilename+".pem")
	if err != nil {
		return "", fmt.Errorf("failed to create form file: %v", err)
	}

	_, err = part.Write(keyData)
	if err != nil {
		return "", fmt.Errorf("failed to write key data: %v", err)
	}

	err = writer.Close()
	if err != nil {
		return "", fmt.Errorf("failed to close multipart writer: %v", err)
	}

	encodedKeyname := url.QueryEscape(ref)
	ipfsAPIKeyImport := fmt.Sprintf("%s/api/v0/key/import?arg=%s&format=pem-pkcs8-cleartext", c.Address, encodedKeyname)

	req, err := http.NewRequest("POST", ipfsAPIKeyImport, body)
	if err != nil {
		return "", fmt.Errorf("failed to create HTTP request: %v", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())

	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("IPFS API returned error status: %d, body: %s\nRequest URL: %s", resp.StatusCode, string(respBody), ipfsAPIKeyImport)
	}

	return c.getKeyIDFromIPFS(ref)
}

// getKeyIDFromIPFS checks if a key with the given name already exists in IPFS
func (c *Client) getKeyIDFromIPFS(name string) (string, error) {
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}

	ipfsAPIKeyList := c.Address + "/api/v0/key/list"
	req, err := http.NewRequest("POST", ipfsAPIKeyList, nil)
	if err != nil {
		return "", err
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to get key list: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("IPFS API returned error status: %d, body: %s\nRequest URL: %s", resp.StatusCode, string(respBody), ipfsAPIKeyList)
	}

	var result struct {
		Keys []struct {
			Name string `json:"name"`
			ID   string `json:"id"`
		} `json:"Keys"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("failed to decode response: %v", err)
	}

	for _, key := range result.Keys {
		if key.Name == name {
			return key.ID, nil
		}
	}

	return "", fmt.Errorf("key not found: %s", name)
}

func (c *Client) IsRef(s string) bool {
	parts := strings.Split(s, "/")
	lastPart := parts[len(parts)-1]

	if strings.Contains(lastPart, ":") || strings.Contains(lastPart, "@") {
		return true
	}

	return len(parts) >= 2
}
