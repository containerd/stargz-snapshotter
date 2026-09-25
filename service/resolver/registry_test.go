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

package resolver

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/reference"
)

const testRegistryHost = "registry.example.com"

type testCertBundle struct {
	caCert     *x509.Certificate
	caKey      *ecdsa.PrivateKey
	serverCert tls.Certificate
	clientCert tls.Certificate
}

func generateTestCertBundle(t *testing.T, serverHost string) testCertBundle {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: serverHost},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(serverHost); ip != nil {
		serverTemplate.IPAddresses = []net.IP{ip}
	} else {
		serverTemplate.DNSNames = []string{serverHost}
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	serverCert, err := tls.X509KeyPair(
		pemEncodeCertificate(serverDER),
		pemEncodePrivateKey(serverKey),
	)
	if err != nil {
		t.Fatal(err)
	}

	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caCert, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	clientCert, err := tls.X509KeyPair(
		pemEncodeCertificate(clientDER),
		pemEncodePrivateKey(clientKey),
	)
	if err != nil {
		t.Fatal(err)
	}

	return testCertBundle{
		caCert:     caCert,
		caKey:      caKey,
		serverCert: serverCert,
		clientCert: clientCert,
	}
}

func pemEncodeCertificate(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func pemEncodePrivateKey(key *ecdsa.PrivateKey) []byte {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

func writeCertBundleFiles(t *testing.T, dir string, bundle testCertBundle) (caPath, clientCertPath, clientKeyPath string) {
	t.Helper()

	caPath = filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, pemEncodeCertificate(bundle.caCert.Raw), 0o600); err != nil {
		t.Fatal(err)
	}
	clientCertPath = filepath.Join(dir, "client.pem")
	clientKeyPath = filepath.Join(dir, "client.key")
	if err := os.WriteFile(clientCertPath, pemEncodeCertificate(bundle.clientCert.Certificate[0]), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clientKeyPath, pemEncodePrivateKey(bundle.clientCert.PrivateKey.(*ecdsa.PrivateKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	return caPath, clientCertPath, clientKeyPath
}

func startMTLSTestServer(t *testing.T, bundle testCertBundle) *httptest.Server {
	t.Helper()

	clientCAPool := x509.NewCertPool()
	clientCAPool.AddCert(bundle.caCert)

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{bundle.serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAPool,
		MinVersion:   tls.VersionTLS12,
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func testReference(host string) reference.Spec {
	ref, err := reference.Parse(host + "/repo:latest")
	if err != nil {
		panic(err)
	}
	return ref
}

func TestRegistryHostsFromConfigInlineTLS(t *testing.T) {
	bundle := generateTestCertBundle(t, "127.0.0.1")
	server := startMTLSTestServer(t, bundle)
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	certDir := t.TempDir()
	caPath, clientCertPath, clientKeyPath := writeCertBundleFiles(t, certDir, bundle)

	hostsFn := RegistryHostsFromConfig(Config{
		Host: map[string]HostConfig{
			testRegistryHost: {
				Mirrors: []MirrorConfig{{
					Host: serverURL.Host,
					TLS: &TLSConfig{
						CAFile:   caPath,
						CertFile: clientCertPath,
						KeyFile:  clientKeyPath,
					},
				}},
			},
		},
	})
	hosts, err := hostsFn(testReference(testRegistryHost))
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) < 1 {
		t.Fatalf("expected at least 1 host, got %d", len(hosts))
	}

	host := hosts[0]
	if host.Host != serverURL.Host {
		t.Fatalf("expected host %q, got %q", serverURL.Host, host.Host)
	}

	resp, err := host.Client.Get(server.URL)
	if err != nil {
		t.Fatalf("expected mTLS request to succeed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
}

func TestRegistryHostsFromConfigInlineTLSError(t *testing.T) {
	certDir := t.TempDir()
	certPath := filepath.Join(certDir, "client.pem")
	if err := os.WriteFile(certPath, []byte("not-a-cert"), 0o600); err != nil {
		t.Fatal(err)
	}

	hostsFn := RegistryHostsFromConfig(Config{
		Host: map[string]HostConfig{
			testRegistryHost: {
				Mirrors: []MirrorConfig{{
					Host: testRegistryHost,
					TLS: &TLSConfig{
						CertFile: certPath,
						KeyFile:  filepath.Join(certDir, "missing.key"),
					},
				}},
			},
		},
	})
	_, err := hostsFn(testReference(testRegistryHost))
	if err == nil {
		t.Fatal("expected TLS configuration error")
	}
}
