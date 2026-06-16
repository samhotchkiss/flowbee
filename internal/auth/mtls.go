package auth

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
)

// MTLSConfig describes the files needed to run the private worker API under
// mutual TLS (DESIGN §7.6: "mTLS client certs ... against a Flowbee allowlist of
// enrolled identities"). This is the production cross-box substrate; it requires
// a CA plus per-worker client certs that are real infra, so per the M12
// "documented, not required in-env" carve-out it is wired and unit-tested for its
// config-building logic but NOT exercised by the cross-box acceptance test (that
// path uses the bearer token over a non-loopback listener instead).
//
//	tlsCfg, err := MTLSConfig{
//	    ServerCertFile: "server.crt", ServerKeyFile: "server.key",
//	    ClientCAFile:   "worker-ca.crt",
//	}.ServerTLS()
//	srv := &http.Server{Addr: ":7070", Handler: h, TLSConfig: tlsCfg}
//	srv.ListenAndServeTLS("", "")  // certs come from tlsCfg
//
// The client cert's CommonName is the enrolled identity; MTLSIdentity extracts it
// for the same enrolled-allowlist check the bearer path performs.
type MTLSConfig struct {
	ServerCertFile string
	ServerKeyFile  string
	ClientCAFile   string
}

// ServerTLS builds a *tls.Config that REQUIRES and verifies a client cert signed
// by ClientCAFile — the mTLS trust boundary. A worker without an enrolled client
// cert cannot complete the handshake, so it never reaches the worker API.
func (c MTLSConfig) ServerTLS() (*tls.Config, error) {
	if c.ServerCertFile == "" || c.ServerKeyFile == "" || c.ClientCAFile == "" {
		return nil, errors.New("mtls: server cert, key, and client CA are all required")
	}
	cert, err := tls.LoadX509KeyPair(c.ServerCertFile, c.ServerKeyFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: load server keypair: %w", err)
	}
	caPEM, err := os.ReadFile(c.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: read client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("mtls: client CA file contained no usable certificates")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// MTLSIdentity extracts the enrolled identity (the client cert CommonName) from a
// request that arrived over a verified mTLS connection. It returns false if the
// request did not present a verified client cert.
func MTLSIdentity(r *http.Request) (string, bool) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		return "", false
	}
	leaf := r.TLS.VerifiedChains[0][0]
	if leaf.Subject.CommonName == "" {
		return "", false
	}
	return leaf.Subject.CommonName, true
}

// MTLSAuth adapts mTLS into the Authenticator interface: the verified client
// cert's CommonName must be an enrolled identity. Used identically to BearerAuth
// when the server runs under MTLSConfig.ServerTLS().
type MTLSAuth struct {
	enrolled map[string]struct{}
}

// NewMTLS builds an mTLS authenticator over the enrolled-identity allowlist.
func NewMTLS(enrolled []string) *MTLSAuth {
	set := make(map[string]struct{}, len(enrolled))
	for _, id := range enrolled {
		set[id] = struct{}{}
	}
	return &MTLSAuth{enrolled: set}
}

// Authenticate verifies the connection presented an enrolled client cert.
func (m *MTLSAuth) Authenticate(r *http.Request) (string, error) {
	id, ok := MTLSIdentity(r)
	if !ok {
		return "", ErrUnauthorized
	}
	if _, ok := m.enrolled[id]; !ok {
		return "", ErrUnauthorized
	}
	return id, nil
}
