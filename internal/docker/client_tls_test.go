package docker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mobyclient "github.com/moby/moby/client"
)

const tlsTestAPIVersion = "1.48"

func TestNewMobyClientTCPTransportMatrix(t *testing.T) {
	tests := []struct {
		name          string
		tlsVerify     bool
		certMode      string
		wantError     bool
		wantTransport string
		wantTLS       bool
	}{
		{name: "verification disabled without certificates uses HTTP", certMode: "empty", wantTransport: "http"},
		{name: "verification disabled with certificates uses TLS", certMode: "valid", wantTransport: "https", wantTLS: true},
		{name: "verification disabled with missing certificates fails", certMode: "missing", wantError: true},
		{name: "verification enabled without certificates fails", tlsVerify: true, certMode: "empty", wantError: true},
		{name: "verification enabled with certificates uses TLS", tlsVerify: true, certMode: "valid", wantTransport: "https", wantTLS: true},
		{name: "verification enabled with missing certificates fails", tlsVerify: true, certMode: "missing", wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tlsTestPreserveDockerEnvironment(t)
			tlsTestClearDockerEnvironment(t)
			tlsTestResetSharedClient(t)

			stats := &tlsTestDockerAPIStats{}
			var server *httptest.Server
			certPath := ""
			switch tt.certMode {
			case "valid":
				pki := tlsTestNewPKI(t, strings.ReplaceAll(tt.name, " ", "-"))
				certPath = filepath.Join(t.TempDir(), "certs")
				pki.writeDockerClientDirectory(t, certPath)
				server = tlsTestNewMTLSServer(t, pki.serverCertificate, pki.caPool, stats)
			case "missing":
				certPath = filepath.Join(t.TempDir(), "missing-certs")
				server = tlsTestNewPlainServer(t, stats)
			default:
				server = tlsTestNewPlainServer(t, stats)
			}

			verify := tt.tlsVerify
			host := tlsTestDockerHost(t, server)
			Configure(Options{
				Host:       host,
				TLSVerify:  &verify,
				CertPath:   certPath,
				APIVersion: tlsTestAPIVersion,
			})

			cli, info, err := NewMobyClientWithInfo()
			if tt.wantError {
				if err == nil && cli != nil {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					_, _ = cli.Ping(ctx, mobyclient.PingOptions{})
					cancel()
				}
				if err == nil {
					t.Errorf("NewMobyClientWithInfo() error = nil, want TLS configuration error; info=%#v", info)
				}
				if cli != nil {
					t.Errorf("NewMobyClientWithInfo() client = %p, want nil after configuration error", cli)
				}
				if tt.certMode == "missing" && err != nil && !strings.Contains(err.Error(), certPath) {
					t.Errorf("error = %q, want missing certificate path %q", err, certPath)
				}
				if tt.tlsVerify && tt.certMode == "empty" && err != nil && !strings.Contains(strings.ToLower(err.Error()), "cert") {
					t.Errorf("error = %q, want explicit certificate-path requirement", err)
				}
				if got := stats.hits.Load(); got != 0 {
					t.Errorf("Docker API requests = %d, want 0 after early TLS failure (possible plaintext downgrade)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewMobyClientWithInfo() error = %v", err)
			}
			if cli == nil {
				t.Fatal("NewMobyClientWithInfo() client = nil")
			}

			tlsTestAssertConnectionInfo(t, info, host, tt.wantTransport, tt.wantTLS, tt.tlsVerify, certPath)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if _, err := cli.Ping(ctx, mobyclient.PingOptions{}); err != nil {
				t.Fatalf("Ping() error = %v", err)
			}
			if got := stats.hits.Load(); got == 0 {
				t.Fatal("Docker API requests = 0, want successful ping")
			}
			if tt.wantTLS {
				if got := stats.tlsHits.Load(); got != stats.hits.Load() {
					t.Errorf("TLS requests = %d, total requests = %d", got, stats.hits.Load())
				}
				if got := stats.verifiedClientCertificateHits.Load(); got != stats.hits.Load() {
					t.Errorf("verified client-certificate requests = %d, total requests = %d", got, stats.hits.Load())
				}
			} else if got := stats.tlsHits.Load(); got != 0 {
				t.Errorf("TLS requests = %d, want plain HTTP", got)
			}
		})
	}
}

func TestNewMobyClientRejectsUnsupportedAndInvalidTLSTransports(t *testing.T) {
	tests := []struct {
		name      string
		host      string
		tlsVerify bool
		certPath  string
	}{
		{name: "HTTP URL", host: "http://docker.example:2375"},
		{name: "HTTPS URL", host: "https://docker.example:2376"},
		{name: "SSH URL", host: "ssh://docker.example"},
		{name: "unknown URL", host: "custom://docker.example"},
		{name: "TLS verification over unix socket", host: "unix:///var/run/docker.sock", tlsVerify: true},
		{name: "TLS certificates over named pipe", host: "npipe:////./pipe/docker_engine", certPath: "unused-cert-path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tlsTestPreserveDockerEnvironment(t)
			tlsTestClearDockerEnvironment(t)
			tlsTestResetSharedClient(t)

			verify := tt.tlsVerify
			Configure(Options{Host: tt.host, TLSVerify: &verify, CertPath: tt.certPath})
			cli, info, err := NewMobyClientWithInfo()
			if err == nil || cli != nil {
				t.Fatalf("NewMobyClientWithInfo() = (%p, %#v, %v), want nil client and transport error", cli, info, err)
			}
			if !strings.Contains(err.Error(), tt.host) {
				t.Errorf("error = %q, want rejected host %q", err, tt.host)
			}
		})
	}
}

func TestNewMobyClientNegotiatesAPIVersionWhenUnset(t *testing.T) {
	tlsTestPreserveDockerEnvironment(t)
	tlsTestClearDockerEnvironment(t)
	tlsTestResetSharedClient(t)

	stats := &tlsTestDockerAPIStats{}
	server := tlsTestNewPlainServer(t, stats)
	host := tlsTestDockerHost(t, server)
	Configure(Options{Host: host})

	cli, info, err := NewMobyClientWithInfo()
	if err != nil {
		t.Fatalf("NewMobyClientWithInfo() error = %v", err)
	}
	if info.APIVersion != "" {
		t.Fatalf("ConnectionInfo.APIVersion = %q, want empty configured version", info.APIVersion)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx, mobyclient.PingOptions{NegotiateAPIVersion: true}); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	if got := cli.ClientVersion(); got != tlsTestAPIVersion {
		t.Fatalf("ClientVersion() after negotiation = %q, want %q", got, tlsTestAPIVersion)
	}
}

func TestNewMobyClientTLSVerificationRejectsWrongCA(t *testing.T) {
	tlsTestPreserveDockerEnvironment(t)
	tlsTestClearDockerEnvironment(t)
	tlsTestResetSharedClient(t)

	clientPKI := tlsTestNewPKI(t, "client-ca")
	serverPKI := tlsTestNewPKI(t, "server-ca")
	certPath := filepath.Join(t.TempDir(), "client-certs")
	clientPKI.writeDockerClientDirectory(t, certPath)
	stats := &tlsTestDockerAPIStats{}
	server := tlsTestNewMTLSServer(t, serverPKI.serverCertificate, clientPKI.caPool, stats)
	host := tlsTestDockerHost(t, server)

	verify := true
	Configure(Options{Host: host, TLSVerify: &verify, CertPath: certPath, APIVersion: tlsTestAPIVersion})
	verifiedClient, verifiedInfo, err := NewMobyClientWithInfo()
	if err != nil {
		t.Fatalf("NewMobyClientWithInfo() error = %v, want valid client certificate material", err)
	}
	tlsTestAssertConnectionInfo(t, verifiedInfo, host, "https", true, true, certPath)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, pingErr := verifiedClient.Ping(ctx, mobyclient.PingOptions{})
	cancel()
	if pingErr == nil {
		t.Fatal("Ping() error = nil, want server certificate verification failure")
	}
	lowerError := strings.ToLower(pingErr.Error())
	if !strings.Contains(lowerError, "x509") && !strings.Contains(lowerError, "certificate") {
		t.Fatalf("Ping() error = %v, want x509 certificate error", pingErr)
	}
	if got := stats.hits.Load(); got != 0 {
		t.Fatalf("Docker API handler requests = %d, want 0 before verified TLS handshake completes", got)
	}

	verify = false
	Configure(Options{Host: host, TLSVerify: &verify, CertPath: certPath, APIVersion: tlsTestAPIVersion})
	unverifiedClient, unverifiedInfo, err := NewMobyClientWithInfo()
	if err != nil {
		t.Fatalf("NewMobyClientWithInfo() with verification disabled error = %v", err)
	}
	if unverifiedClient == verifiedClient {
		t.Fatal("TLS verification change reused cached client")
	}
	tlsTestAssertConnectionInfo(t, unverifiedInfo, host, "https", true, false, certPath)
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := unverifiedClient.Ping(ctx, mobyclient.PingOptions{}); err != nil {
		t.Fatalf("Ping() with verification disabled error = %v", err)
	}
	if got := stats.tlsHits.Load(); got == 0 {
		t.Fatal("TLS requests = 0, want TLS transport even when verification is disabled")
	}
	if got := stats.verifiedClientCertificateHits.Load(); got == 0 {
		t.Fatal("verified client-certificate requests = 0, want mTLS client authentication")
	}
}

func TestNewMobyClientRejectsDamagedCertificateMaterial(t *testing.T) {
	tlsTestPreserveDockerEnvironment(t)
	tlsTestClearDockerEnvironment(t)
	tlsTestResetSharedClient(t)

	pki := tlsTestNewPKI(t, "damaged-client-certificate")
	certPath := filepath.Join(t.TempDir(), "certs")
	pki.writeDockerClientDirectory(t, certPath)
	if err := os.WriteFile(filepath.Join(certPath, "cert.pem"), []byte("not a PEM certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stats := &tlsTestDockerAPIStats{}
	server := tlsTestNewMTLSServer(t, pki.serverCertificate, pki.caPool, stats)
	verify := true
	Configure(Options{
		Host:       tlsTestDockerHost(t, server),
		TLSVerify:  &verify,
		CertPath:   certPath,
		APIVersion: tlsTestAPIVersion,
	})

	cli, _, err := NewMobyClientWithInfo()
	if err == nil {
		t.Fatal("NewMobyClientWithInfo() error = nil, want damaged certificate error")
	}
	if cli != nil {
		t.Fatalf("NewMobyClientWithInfo() client = %p, want nil", cli)
	}
	if got := stats.hits.Load(); got != 0 {
		t.Fatalf("Docker API requests = %d, want 0 after certificate parse failure", got)
	}
}

func TestNewMobyClientRejectsDamagedCAWhenVerificationIsDisabled(t *testing.T) {
	tlsTestPreserveDockerEnvironment(t)
	tlsTestClearDockerEnvironment(t)
	tlsTestResetSharedClient(t)

	pki := tlsTestNewPKI(t, "damaged-ca")
	certPath := filepath.Join(t.TempDir(), "certs")
	pki.writeDockerClientDirectory(t, certPath)
	if err := os.WriteFile(filepath.Join(certPath, "ca.pem"), []byte("not a PEM certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stats := &tlsTestDockerAPIStats{}
	server := tlsTestNewMTLSServer(t, pki.serverCertificate, pki.caPool, stats)
	verify := false
	Configure(Options{
		Host:       tlsTestDockerHost(t, server),
		TLSVerify:  &verify,
		CertPath:   certPath,
		APIVersion: tlsTestAPIVersion,
	})

	cli, _, err := NewMobyClientWithInfo()
	if err == nil || cli != nil {
		t.Fatalf("NewMobyClientWithInfo() = (%p, %v), want damaged CA error", cli, err)
	}
	if !strings.Contains(err.Error(), "ca.pem") {
		t.Fatalf("error = %q, want ca.pem path", err)
	}
	if got := stats.hits.Load(); got != 0 {
		t.Fatalf("Docker API requests = %d, want 0 after CA parse failure", got)
	}
}

func TestNewMobyClientRestoresDockerEnvironment(t *testing.T) {
	tests := []struct {
		name      string
		wantError bool
	}{
		{name: "success"},
		{name: "configuration error", wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tlsTestPreserveDockerEnvironment(t)
			tlsTestResetSharedClient(t)

			tlsTestSetEnvironment(t, mobyclient.EnvOverrideHost, "tcp://ambient.invalid:2375", true)
			tlsTestSetEnvironment(t, mobyclient.EnvTLSVerify, "ambient-value", true)
			tlsTestSetEnvironment(t, mobyclient.EnvOverrideCertPath, "", false)
			tlsTestSetEnvironment(t, mobyclient.EnvOverrideAPIVersion, "", true)

			pki := tlsTestNewPKI(t, "environment-restore")
			stats := &tlsTestDockerAPIStats{}
			server := tlsTestNewMTLSServer(t, pki.serverCertificate, pki.caPool, stats)
			certPath := filepath.Join(t.TempDir(), "certs")
			verify := false
			if tt.wantError {
				certPath = filepath.Join(t.TempDir(), "missing-certs")
				verify = true
			} else {
				pki.writeDockerClientDirectory(t, certPath)
			}
			Configure(Options{
				Host:       tlsTestDockerHost(t, server),
				TLSVerify:  &verify,
				CertPath:   certPath,
				APIVersion: tlsTestAPIVersion,
			})
			before := tlsTestSnapshotDockerEnvironment()

			cli, _, err := NewMobyClientWithInfo()
			if tt.wantError {
				if err == nil || cli != nil {
					t.Errorf("NewMobyClientWithInfo() = (%p, %v), want nil client and error", cli, err)
				}
			} else if err != nil || cli == nil {
				t.Errorf("NewMobyClientWithInfo() = (%p, %v), want client and nil error", cli, err)
			}
			tlsTestAssertDockerEnvironment(t, before)
		})
	}
}

func TestDockerEnvironmentChangeDoesNotReuseCachedClient(t *testing.T) {
	tlsTestPreserveDockerEnvironment(t)
	tlsTestClearDockerEnvironment(t)
	tlsTestResetSharedClient(t)

	plainStats := &tlsTestDockerAPIStats{}
	plainServer := tlsTestNewPlainServer(t, plainStats)
	tlsTestSetEnvironment(t, mobyclient.EnvOverrideHost, tlsTestDockerHost(t, plainServer), true)
	tlsTestSetEnvironment(t, mobyclient.EnvOverrideAPIVersion, tlsTestAPIVersion, true)

	plainClient, plainInfo, err := NewMobyClientWithInfo()
	if err != nil {
		t.Fatalf("NewMobyClientWithInfo() for HTTP endpoint error = %v", err)
	}
	tlsTestAssertConnectionInfo(t, plainInfo, tlsTestDockerHost(t, plainServer), "http", false, false, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	if _, err := plainClient.Ping(ctx, mobyclient.PingOptions{}); err != nil {
		cancel()
		t.Fatalf("HTTP Ping() error = %v", err)
	}
	cancel()
	plainHits := plainStats.hits.Load()

	pki := tlsTestNewPKI(t, "environment-change")
	certPath := filepath.Join(t.TempDir(), "certs")
	pki.writeDockerClientDirectory(t, certPath)
	tlsStats := &tlsTestDockerAPIStats{}
	tlsServer := tlsTestNewMTLSServer(t, pki.serverCertificate, pki.caPool, tlsStats)
	tlsTestSetEnvironment(t, mobyclient.EnvOverrideHost, tlsTestDockerHost(t, tlsServer), true)
	tlsTestSetEnvironment(t, mobyclient.EnvTLSVerify, "1", true)
	tlsTestSetEnvironment(t, mobyclient.EnvOverrideCertPath, certPath, true)
	beforeSecondCall := tlsTestSnapshotDockerEnvironment()

	tlsClient, tlsInfo, err := NewMobyClientWithInfo()
	if err != nil {
		t.Fatalf("NewMobyClientWithInfo() after environment change error = %v", err)
	}
	if tlsClient == plainClient {
		t.Fatal("DOCKER_* environment change reused cached HTTP client")
	}
	tlsTestAssertConnectionInfo(t, tlsInfo, tlsTestDockerHost(t, tlsServer), "https", true, true, certPath)
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := tlsClient.Ping(ctx, mobyclient.PingOptions{}); err != nil {
		t.Fatalf("TLS Ping() after environment change error = %v", err)
	}
	if got := tlsStats.tlsHits.Load(); got == 0 {
		t.Fatal("TLS requests = 0, cached HTTP client may have been reused")
	}
	if got := plainStats.hits.Load(); got != plainHits {
		t.Errorf("old HTTP endpoint requests changed from %d to %d", plainHits, got)
	}
	tlsTestAssertDockerEnvironment(t, beforeSecondCall)
}

func TestFailedClientInitializationIsNotCached(t *testing.T) {
	tlsTestPreserveDockerEnvironment(t)
	tlsTestClearDockerEnvironment(t)
	tlsTestResetSharedClient(t)

	pki := tlsTestNewPKI(t, "retry-after-failure")
	stats := &tlsTestDockerAPIStats{}
	server := tlsTestNewMTLSServer(t, pki.serverCertificate, pki.caPool, stats)
	certPath := filepath.Join(t.TempDir(), "late-certs")
	verify := true
	Configure(Options{
		Host:       tlsTestDockerHost(t, server),
		TLSVerify:  &verify,
		CertPath:   certPath,
		APIVersion: tlsTestAPIVersion,
	})

	failedClient, _, err := NewMobyClientWithInfo()
	if err == nil || failedClient != nil {
		t.Fatalf("first NewMobyClientWithInfo() = (%p, %v), want nil client and error", failedClient, err)
	}
	dockerClientMu.Lock()
	cachedAfterFailure := mobyClient
	dockerClientMu.Unlock()
	if cachedAfterFailure != nil {
		t.Fatalf("mobyClient = %p after initialization failure, want nil", cachedAfterFailure)
	}

	pki.writeDockerClientDirectory(t, certPath)
	retriedClient, retriedInfo, err := NewMobyClientWithInfo()
	if err != nil {
		t.Fatalf("second NewMobyClientWithInfo() after repairing same path error = %v", err)
	}
	if retriedClient == nil {
		t.Fatal("second NewMobyClientWithInfo() client = nil")
	}
	tlsTestAssertConnectionInfo(t, retriedInfo, tlsTestDockerHost(t, server), "https", true, true, certPath)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := retriedClient.Ping(ctx, mobyclient.PingOptions{}); err != nil {
		t.Fatalf("Ping() after repairing same certificate path error = %v", err)
	}
	if got := stats.tlsHits.Load(); got == 0 {
		t.Fatal("TLS requests = 0, want successful retry with newly available certificates")
	}
}

type tlsTestDockerAPIStats struct {
	hits                          atomic.Int64
	tlsHits                       atomic.Int64
	verifiedClientCertificateHits atomic.Int64
}

func tlsTestNewPlainServer(t *testing.T, stats *tlsTestDockerAPIStats) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(tlsTestDockerAPIHandler(stats))
	t.Cleanup(server.Close)
	return server
}

func tlsTestNewMTLSServer(t *testing.T, certificate tls.Certificate, clientCAs *x509.CertPool, stats *tlsTestDockerAPIStats) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(tlsTestDockerAPIHandler(stats))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{certificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		MinVersion:   tls.VersionTLS12,
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func tlsTestDockerAPIHandler(stats *tlsTestDockerAPIStats) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stats.hits.Add(1)
		if r.TLS != nil {
			stats.tlsHits.Add(1)
			if len(r.TLS.PeerCertificates) > 0 && len(r.TLS.VerifiedChains) > 0 {
				stats.verifiedClientCertificateHits.Add(1)
			}
		}
		switch {
		case r.URL.Path == "/_ping":
			w.Header().Set("API-Version", tlsTestAPIVersion)
			w.Header().Set("OSType", "linux")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "OK")
		case strings.HasSuffix(r.URL.Path, "/version"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"Version":"28.1.1","ApiVersion":"1.48","MinAPIVersion":"1.24","Os":"linux","Arch":"amd64"}`)
		default:
			http.NotFound(w, r)
		}
	})
}

type tlsTestPKI struct {
	caPEM                []byte
	clientCertificatePEM []byte
	clientKeyPEM         []byte
	serverCertificate    tls.Certificate
	caPool               *x509.CertPool
}

func tlsTestNewPKI(t *testing.T, name string) tlsTestPKI {
	t.Helper()
	_, caKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name + " root CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCertificate, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		t.Fatal("append test CA certificate")
	}

	serverCertificate, _, _ := tlsTestIssueCertificate(t, caCertificate, caKey, x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	})
	_, clientCertificatePEM, clientKeyPEM := tlsTestIssueCertificate(t, caCertificate, caKey, x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: name + " client"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})

	return tlsTestPKI{
		caPEM:                caPEM,
		clientCertificatePEM: clientCertificatePEM,
		clientKeyPEM:         clientKeyPEM,
		serverCertificate:    serverCertificate,
		caPool:               caPool,
	}
}

func tlsTestIssueCertificate(t *testing.T, ca *x509.Certificate, caKey ed25519.PrivateKey, template x509.Certificate) (tls.Certificate, []byte, []byte) {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, ca, privateKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	privateKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER})
	certificate, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, certificatePEM, privateKeyPEM
}

func (p tlsTestPKI) writeDockerClientDirectory(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"ca.pem":   p.caPEM,
		"cert.pem": p.clientCertificatePEM,
		"key.pem":  p.clientKeyPEM,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(path, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func tlsTestDockerHost(t *testing.T, server *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return "tcp://" + u.Host
}

func tlsTestAssertConnectionInfo(t *testing.T, info ConnectionInfo, host, transport string, usesTLS, verifiesTLS bool, certPath string) {
	t.Helper()
	if info.Host != host {
		t.Errorf("ConnectionInfo.Host = %q, want %q", info.Host, host)
	}
	if info.Transport != transport {
		t.Errorf("ConnectionInfo.Transport = %q, want %q", info.Transport, transport)
	}
	if info.TLS != usesTLS {
		t.Errorf("ConnectionInfo.TLS = %t, want %t", info.TLS, usesTLS)
	}
	if info.TLSVerify != verifiesTLS {
		t.Errorf("ConnectionInfo.TLSVerify = %t, want %t", info.TLSVerify, verifiesTLS)
	}
	if info.CertPath != certPath {
		t.Errorf("ConnectionInfo.CertPath = %q, want %q", info.CertPath, certPath)
	}
	wantClientCertificate := certPath != ""
	if info.ClientCertificate != wantClientCertificate {
		t.Errorf("ConnectionInfo.ClientCertificate = %t, want %t", info.ClientCertificate, wantClientCertificate)
	}
	if usesTLS && info.CASource == "" {
		t.Error("ConnectionInfo.CASource is empty for TLS transport")
	}
	if info.APIVersion != tlsTestAPIVersion {
		t.Errorf("ConnectionInfo.APIVersion = %q, want %q", info.APIVersion, tlsTestAPIVersion)
	}
}

type tlsTestEnvironmentValue struct {
	value string
	set   bool
}

var tlsTestDockerEnvironmentKeys = []string{
	mobyclient.EnvOverrideHost,
	mobyclient.EnvTLSVerify,
	mobyclient.EnvOverrideCertPath,
	mobyclient.EnvOverrideAPIVersion,
}

func tlsTestPreserveDockerEnvironment(t *testing.T) {
	t.Helper()
	snapshot := tlsTestSnapshotDockerEnvironment()
	t.Cleanup(func() {
		for _, key := range tlsTestDockerEnvironmentKeys {
			value := snapshot[key]
			var err error
			if value.set {
				err = os.Setenv(key, value.value)
			} else {
				err = os.Unsetenv(key)
			}
			if err != nil {
				t.Errorf("restore %s: %v", key, err)
			}
		}
	})
}

func tlsTestClearDockerEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range tlsTestDockerEnvironmentKeys {
		tlsTestSetEnvironment(t, key, "", false)
	}
}

func tlsTestSetEnvironment(t *testing.T, key, value string, set bool) {
	t.Helper()
	var err error
	if set {
		err = os.Setenv(key, value)
	} else {
		err = os.Unsetenv(key)
	}
	if err != nil {
		t.Fatalf("set test environment %s: %v", key, err)
	}
}

func tlsTestSnapshotDockerEnvironment() map[string]tlsTestEnvironmentValue {
	snapshot := make(map[string]tlsTestEnvironmentValue, len(tlsTestDockerEnvironmentKeys))
	for _, key := range tlsTestDockerEnvironmentKeys {
		value, set := os.LookupEnv(key)
		snapshot[key] = tlsTestEnvironmentValue{value: value, set: set}
	}
	return snapshot
}

func tlsTestAssertDockerEnvironment(t *testing.T, want map[string]tlsTestEnvironmentValue) {
	t.Helper()
	for _, key := range tlsTestDockerEnvironmentKeys {
		gotValue, gotSet := os.LookupEnv(key)
		got := tlsTestEnvironmentValue{value: gotValue, set: gotSet}
		if got != want[key] {
			t.Errorf("environment %s = %#v, want %#v", key, got, want[key])
		}
	}
}

func tlsTestResetSharedClient(t *testing.T) {
	t.Helper()
	reset := func() {
		Configure(Options{Host: "tcp://tls-test-reset.invalid:1"})
		Configure(Options{})
	}
	reset()
	t.Cleanup(reset)
}
