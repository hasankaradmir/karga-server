//go:build integration

package integration

import (
	"context"
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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/hasankaradmir/karga-sdk/packages"

	"github.com/hasankaradmir/karga-server/internal/application/gateway"
	"github.com/hasankaradmir/karga-server/internal/application/releases"
)

// selfSigned returns a certificate for host and a values file serving it.
func selfSigned(t *testing.T, host string, serial int64) (*x509.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	vals, _ := yaml.Marshal(map[string]any{"tls": map[string]string{
		"cert": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		"key":  string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
	}})
	file := filepath.Join(t.TempDir(), "tls.yaml")
	if err := os.WriteFile(file, vals, 0o600); err != nil {
		t.Fatal(err)
	}
	return cert, file
}

// httpsGet requests https://host:port/ through 127.0.0.1, trusting only cert, and returns the body
// and the certificate the gateway presented.
func httpsGet(t *testing.T, port int, host string, cert *x509.Certificate) (string, *x509.Certificate) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:"+strconv.Itoa(port))
		},
	}}
	resp, err := client.Get("https://" + host + ":" + strconv.Itoa(port) + "/")
	if err != nil {
		t.Fatalf("https %s: %v", host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(b)), resp.TLS.PeerCertificates[0]
}

func TestGatewayTLS(t *testing.T) {
	requireLinux(t)
	ctx := context.Background()
	name := releaseName("it-gw-tls")
	s, _ := service(t, name)
	l, port := listener(t, gateway.ProtocolHTTPS)
	const host = "secure.local"
	cert1, vals1 := selfSigned(t, host, 1)
	sets := []string{"route=" + host, "routeListener=" + l}
	if _, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: webPkg,
		Values: releases.Values{Files: [][]byte{readFile(t, vals1)}, Sets: packages.Sets(sets...)}}, nil); err != nil {
		t.Fatal(err)
	}
	if body, got := httpsGet(t, port, host, cert1); body != "v1" || got.SerialNumber.Int64() != 1 {
		t.Fatalf("https: %q serial %v", body, got.SerialNumber)
	}

	// Plain HTTP on port 80 of the same address redirects to HTTPS.
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:80/path", nil)
	req.Host = host
	noFollow := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatalf("redirect: %v", err)
	}
	_ = resp.Body.Close()
	if want := "https://" + host + ":" + strconv.Itoa(port) + "/path"; resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != want {
		t.Fatalf("redirect: %d %q, want 301 %q", resp.StatusCode, resp.Header.Get("Location"), want)
	}

	// A new certificate is served after an upgrade.
	cert2, vals2 := selfSigned(t, host, 2)
	if _, err := s.Lifecycle.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: opts(), Name: name, Package: webPkg,
		Values: releases.Values{Files: [][]byte{readFile(t, vals2)}, Sets: packages.Sets(sets...)}}, nil); err != nil {
		t.Fatal(err)
	}
	if body, got := httpsGet(t, port, host, cert2); body != "v1" || got.SerialNumber.Int64() != 2 {
		t.Fatalf("after rotation: %q serial %v", body, got.SerialNumber)
	}
}
