package inter

import (
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
	"net/http/httptest"
	"testing"
	"time"
)

type pki struct {
	pool                  *x509.CertPool
	clientCert, clientKey []byte
	server                *httptest.Server
}

func newPKI(t *testing.T, notAfter time.Time, h http.Handler, before ...func(*http.Server)) pki {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(2, 0, 0), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	leaf := func(cn string, ip bool, until time.Time, usage x509.ExtKeyUsage) (certPEM, keyPEM []byte) {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: until, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		if ip {
			tpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		}
		der, _ := x509.CreateCertificate(rand.Reader, tpl, ca, &k.PublicKey, caKey)
		kb, _ := x509.MarshalECPrivateKey(k)
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	}
	sc, sk := leaf("127.0.0.1", true, time.Now().AddDate(1, 0, 0), x509.ExtKeyUsageServerAuth)
	cc, ck := leaf("CN=merchant-integration", false, notAfter, x509.ExtKeyUsageClientAuth)
	srv := httptest.NewUnstartedServer(h)
	pair, _ := tls.X509KeyPair(sc, sk)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	for _, f := range before {
		f(srv.Config)
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return pki{pool: pool, clientCert: cc, clientKey: ck, server: srv}
}
