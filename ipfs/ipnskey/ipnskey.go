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

// Package ipnskey provides functionality for generating deterministic Ed25519 key pairs
// for the InterPlanetary Name System (IPNS). It implements key generation based on a
// given name string, ensuring the same name always produces the same key pair,
// which is useful for reproducible builds and consistent IPNS addressing.
//
// For more information about IPNS, see:
// - IPNS (InterPlanetary Name System): https://docs.ipfs.tech/concepts/ipns/
// - IPNS specification: https://specs.ipfs.tech/ipns/ipns-record/
// - Key format: https://docs.ipfs.tech/concepts/ipns/#ipns-keys
package ipnskey

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

type detRand struct {
	data   []byte
	offset int
}

func newDetRand(seed string) *detRand {
	hasher := sha256.New()
	hasher.Write([]byte(seed))
	initial := hasher.Sum(nil)

	data := make([]byte, 8192)
	copy(data, initial)

	for i := 32; i < len(data); i += 32 {
		hasher.Reset()
		hasher.Write(data[i-32 : i])
		copy(data[i:i+32], hasher.Sum(nil))
	}

	return &detRand{
		data:   data,
		offset: 0,
	}
}

func (r *detRand) Read(p []byte) (n int, err error) {
	if r.offset >= len(r.data) {
		hasher := sha256.New()
		hasher.Write(r.data)
		newData := hasher.Sum(nil)
		copy(r.data, newData)
		r.offset = 0
	}

	n = copy(p, r.data[r.offset:])
	r.offset += n
	return n, nil
}

func GenerateKeyData(name string) ([]byte, error) {
	reader := newDetRand(name)

	seedBytes := make([]byte, 32)
	_, err := reader.Read(seedBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to generate seed: %v", err)
	}

	privateKey := ed25519.NewKeyFromSeed(seedBytes)

	privateKeyBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to convert key format: %v", err)
	}

	var pemBuf bytes.Buffer
	err = pem.Encode(&pemBuf, &pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: privateKeyBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to generate PEM data: %v", err)
	}

	return pemBuf.Bytes(), nil
}
