/*
Copyright 2026 The CNMySQL Authors.

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

package webserver

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"
)

// TLSOptions configures the mutual-TLS server.
type TLSOptions struct {
	// ServerCertFile and ServerKeyFile are the server's certificate and key.
	ServerCertFile string
	ServerKeyFile  string
	// ClientCAFile is the CA bundle used to verify operator client certs.
	ClientCAFile string
}

// MTLSConfig builds a tls.Config that requires and verifies a client
// certificate signed by the configured client CA. It is exported so other
// servers (notably the Prometheus metrics endpoint) can adopt the same
// mutual-TLS posture as the control API rather than re-implementing it.
func (o TLSOptions) MTLSConfig() (*tls.Config, error) {
	return o.mtlsConfig()
}

// mtlsConfig builds a tls.Config that requires and verifies a client
// certificate signed by the configured client CA.
func (o TLSOptions) mtlsConfig() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(o.ServerCertFile, o.ServerKeyFile)
	if err != nil {
		return nil, fmt.Errorf("loading server keypair: %w", err)
	}

	caPEM, err := os.ReadFile(o.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("reading client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no valid certificates found in client CA %q", o.ClientCAFile)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// NewServer builds an http.Server that serves the control API over mTLS on the
// given address.
func NewServer(addr string, controller InstanceController, opts TLSOptions) (*http.Server, error) {
	tlsConfig, err := opts.mtlsConfig()
	if err != nil {
		return nil, err
	}

	return &http.Server{
		Addr:              addr,
		Handler:           Handler(controller),
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 10 * time.Second,
	}, nil
}
