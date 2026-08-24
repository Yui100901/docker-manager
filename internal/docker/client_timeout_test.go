package docker

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mobyclient "github.com/moby/moby/client"
)

func TestDockerClientResponseHeaderTimeout(t *testing.T) {
	tlsTestPreserveDockerEnvironment(t)
	tlsTestClearDockerEnvironment(t)
	tlsTestResetSharedClient(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(250 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	timeout := 50 * time.Millisecond
	Configure(Options{Host: tlsTestDockerHost(t, server), APIVersion: tlsTestAPIVersion, Timeout: timeout})
	cli, info, err := NewMobyClientWithInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.Timeout != timeout {
		t.Fatalf("ConnectionInfo.Timeout = %v, want %v", info.Timeout, timeout)
	}
	start := time.Now()
	_, err = cli.Ping(context.Background(), mobyclient.PingOptions{})
	if err == nil {
		t.Fatalf("Ping() error = %v, want response-header timeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("response-header timeout took %v, want bounded request", elapsed)
	}
}

func TestDockerClientTLSHandshakeTimeout(t *testing.T) {
	tlsTestPreserveDockerEnvironment(t)
	tlsTestClearDockerEnvironment(t)
	tlsTestResetSharedClient(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var connections sync.WaitGroup
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer connections.Done()
				defer connection.Close()
				<-stop
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		close(stop)
		connections.Wait()
	})

	pki := tlsTestNewPKI(t, "handshake-timeout")
	certPath := filepath.Join(t.TempDir(), "certs")
	pki.writeDockerClientDirectory(t, certPath)
	timeout := 50 * time.Millisecond
	Configure(Options{
		Host:       "tcp://" + listener.Addr().String(),
		CertPath:   certPath,
		APIVersion: tlsTestAPIVersion,
		Timeout:    timeout,
	})
	cli, _, err := NewMobyClientWithInfo()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = cli.Ping(context.Background(), mobyclient.PingOptions{})
	if err == nil {
		t.Fatalf("Ping() error = %v, want TLS handshake timeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("TLS handshake timeout took %v, want bounded request", elapsed)
	}
}
