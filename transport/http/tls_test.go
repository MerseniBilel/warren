package http_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MerseniBilel/warren"
	whttp "github.com/MerseniBilel/warren/transport/http"
)

// TLSFiles had no test of any kind, and the failure it hides is the one a
// typo in a path produces on the day of a release.

// certPair writes a self-signed certificate and key, and returns their paths.
func certPair(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("certificate: %v", err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

func TestTLSFilesServesHTTPS(t *testing.T) {
	t.Parallel()
	certPath, keyPath := certPair(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	a := warren.New(userModule(), whttp.Server(
		whttp.Listener(ln), whttp.DrainDelay(0), whttp.TLSFiles(certPath, keyPath),
	))
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() {
		if err := a.Stop(context.Background()); err != nil {
			t.Errorf("stop: %v", err)
		}
	})

	pool := x509.NewCertPool()
	pem, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	pool.AppendCertsFromPEM(pem)
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}

	res, err := client.Get("https://" + ln.Addr().String() + "/users/u-1")
	if err != nil {
		t.Fatalf("https request: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 200 {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if res.TLS == nil {
		t.Error("the response did not arrive over TLS")
	}
}

func TestACertificateThatCannotBeLoadedFailsTheBoot(t *testing.T) {
	t.Parallel()
	// The failure this replaces: ServeTLS runs in a goroutine, so a
	// mistyped path let Start return nil, logged "http server listening
	// tls=true", turned readiness green — and the error surfaced at STOP,
	// which on a healthy-looking service is hours later or never. Meanwhile
	// the listener is open and accepts nothing.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	a := warren.New(userModule(), whttp.Server(
		whttp.Listener(ln), whttp.DrainDelay(0),
		whttp.TLSFiles("/nonexistent/cert.pem", "/nonexistent/key.pem"),
	))
	err = a.Start(context.Background())
	if err == nil {
		_ = a.Stop(context.Background())
		t.Fatal("a server with an unreadable certificate started successfully")
	}
	if !strings.Contains(err.Error(), "/nonexistent/cert.pem") {
		t.Errorf("the error does not name the certificate: %v", err)
	}
}

func TestATLSConfigWithNothingToPresentFailsTheBoot(t *testing.T) {
	t.Parallel()
	// The other door: http.TLS(cfg) is what an mTLS mesh uses, and a config
	// whose certificates never got assigned fails every handshake in exactly
	// the same silence.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	a := warren.New(userModule(), whttp.Server(
		whttp.Listener(ln), whttp.DrainDelay(0),
		whttp.TLS(&tls.Config{MinVersion: tls.VersionTLS13}),
	))
	err = a.Start(context.Background())
	if err == nil {
		_ = a.Stop(context.Background())
		t.Fatal("a TLS server with no certificate started successfully")
	}
	if !strings.Contains(err.Error(), "GetCertificate") {
		t.Errorf("the error does not say what to set: %v", err)
	}
}

func TestATLSConfigThatGetsItsCertificateAtHandshakeIsAccepted(t *testing.T) {
	t.Parallel()
	// GetCertificate is how SNI and ACME serve a certificate that does not
	// exist yet at boot. Refusing it would break the case the check exists
	// to protect.
	certPath, keyPath := certPair(t)
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	a := warren.New(userModule(), whttp.Server(
		whttp.Listener(ln), whttp.DrainDelay(0),
		whttp.TLS(&tls.Config{
			MinVersion: tls.VersionTLS12,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				return &pair, nil
			},
		}),
	))
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Errorf("stop: %v", err)
	}
}

func TestACertificateWithoutItsKeyFailsTheBoot(t *testing.T) {
	t.Parallel()
	// Half a pair is the shape a config template produces when one variable
	// is unset. ServeTLS's own error for it is "open : no such file or
	// directory", which names nothing.
	certPath, _ := certPair(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	a := warren.New(userModule(), whttp.Server(
		whttp.Listener(ln), whttp.DrainDelay(0), whttp.TLSFiles(certPath, ""),
	))
	err = a.Start(context.Background())
	if err == nil {
		_ = a.Stop(context.Background())
		t.Fatal("a server with a certificate and no key started successfully")
	}
	if !strings.Contains(err.Error(), "key") {
		t.Errorf("the error does not mention the missing key: %v", err)
	}
}
