package statute

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"testing"
)

func TestACMELeafKeyAlgorithmsPersistAndMatch(t *testing.T) {
	for _, kind := range []string{"", "EC256", "EC384", "RSA2048", "RSA3072", "RSA4096", "RSA8192"} {
		t.Run(kind, func(t *testing.T) {
			key, err := generateLeafKey(kind)
			if err != nil {
				t.Fatal(err)
			}
			data, err := encodeLeafPrivateKey(key)
			if err != nil {
				t.Fatal(err)
			}
			restored := restoreLeafPrivateKey(t, data)
			if !leafKeyMatches(kind, &tls.Certificate{PrivateKey: restored}) {
				t.Fatal("persisted key no longer matches its algorithm")
			}
			if leafKeyMatches("unsupported", &tls.Certificate{PrivateKey: restored}) {
				t.Fatal("unsupported algorithm accepted")
			}
		})
	}
	if _, err := generateLeafKey("unsupported"); err == nil {
		t.Fatal("unsupported key generation accepted")
	}
	if leafKeyMatches("EC256", nil) {
		t.Fatal("nil key matched")
	}
}

func restoreLeafPrivateKey(t *testing.T, data []byte) any {
	t.Helper()
	var err error
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("missing private-key PEM")
	}
	var restored any
	if block.Type == "EC PRIVATE KEY" {
		restored, err = x509.ParseECPrivateKey(block.Bytes)
	} else {
		restored, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	}
	if err != nil {
		t.Fatal(err)
	}
	return restored
}
