package mqtt

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AC-CodeProd/mqtt-raspberry-controller/config"
)

type testCA struct {
	cert    *x509.Certificate
	key     *rsa.PrivateKey
	certPEM []byte
}

func makeTestCA(t *testing.T, name string) testCA {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return testCA{cert: cert, key: key, certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca testCA) issue(t *testing.T, commonName string, dnsNames []string, client bool) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	usage := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	if client {
		usage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: commonName},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: dnsNames,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usage,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return pair, certPEM, keyPEM
}

func writeTLSFile(t *testing.T, name string, contents []byte, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, contents, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func tlsClientConfig(t *testing.T, address string, ca testCA) config.Config {
	t.Helper()
	qos := byte(0)
	return config.Config{
		MQTT: config.MQTTConfig{
			Broker: "tls://" + address, ClientID: "tls-test", BaseTopic: "tls-test", QoS: &qos,
			AllowUnauthenticated: true, Discovery: config.DiscoveryConfig{Prefix: config.DefaultDiscoveryPrefix},
			TLS: config.MQTTTLSConfig{CAFile: writeTLSFile(t, "ca.pem", ca.certPEM, 0644), ServerName: "broker.test"},
		},
		Device:   config.DeviceConfig{ID: "tls-test", Name: "TLS Test"},
		Entities: []config.EntityConfig{{ID: "button", Type: "button", Name: "Button", Press: []config.Action{{Type: "delay", Duration: "1ms"}}}},
	}
}

// startTLSMQTTBroker accepts one MQTT CONNECT and returns a successful CONNACK.
func startTLSMQTTBroker(t *testing.T, tlsConfig *tls.Config) (string, <-chan error) {
	t.Helper()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	result := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			result <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		tlsConn, ok := conn.(*tls.Conn)
		if !ok {
			result <- fmt.Errorf("accepted connection is not TLS")
			return
		}
		if err := tlsConn.Handshake(); err != nil {
			result <- err
			return
		}
		first := []byte{0}
		if _, err := io.ReadFull(conn, first); err != nil {
			result <- err
			return
		}
		if first[0]>>4 != 1 {
			result <- fmt.Errorf("first MQTT packet type = %d, want CONNECT", first[0]>>4)
			return
		}
		remaining := 0
		multiplier := 1
		for {
			b := []byte{0}
			if _, err := io.ReadFull(conn, b); err != nil {
				result <- err
				return
			}
			remaining += int(b[0]&127) * multiplier
			if b[0]&128 == 0 {
				break
			}
			multiplier *= 128
		}
		if _, err := io.CopyN(io.Discard, conn, int64(remaining)); err != nil {
			result <- err
			return
		}
		if _, err := conn.Write([]byte{0x20, 0x02, 0x00, 0x00}); err != nil {
			result <- err
			return
		}
		result <- nil
	}()
	return listener.Addr().String(), result
}

func connectForTLSTest(t *testing.T, cfg config.Config) error {
	t.Helper()
	client, err := New(&cfg)
	if err != nil {
		return err
	}
	if err := client.Connect(); err != nil {
		return err
	}
	client.transport.Disconnect(0)
	return nil
}

func TestTLSCustomCAHandshake(t *testing.T) {
	ca := makeTestCA(t, "custom test CA")
	serverCert, _, _ := ca.issue(t, "broker.test", []string{"broker.test"}, false)
	address, serverResult := startTLSMQTTBroker(t, &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12})
	cfg := tlsClientConfig(t, address, ca)
	if err := connectForTLSTest(t, cfg); err != nil {
		t.Fatalf("custom CA TLS connection failed: %v", err)
	}
	if err := <-serverResult; err != nil {
		t.Fatalf("broker handshake failed: %v", err)
	}
}

func TestTLSRejectsWrongCAHostnameAndMinimumVersion(t *testing.T) {
	ca := makeTestCA(t, "trusted CA")
	wrongCA := makeTestCA(t, "wrong CA")
	serverCert, _, _ := ca.issue(t, "broker.test", []string{"broker.test"}, false)

	tests := []struct {
		name string
		edit func(*config.Config)
	}{
		{"wrong CA", func(cfg *config.Config) { cfg.MQTT.TLS.CAFile = writeTLSFile(t, "wrong-ca.pem", wrongCA.certPEM, 0644) }},
		{"broker hostname mismatch", func(cfg *config.Config) { cfg.MQTT.TLS.ServerName = "" }},
		{"wrong server_name", func(cfg *config.Config) { cfg.MQTT.TLS.ServerName = "wrong.test" }},
		{"TLS minimum", func(cfg *config.Config) { cfg.MQTT.TLS.MinVersion = "1.3" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			address, serverResult := startTLSMQTTBroker(t, &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12})
			cfg := tlsClientConfig(t, address, ca)
			tt.edit(&cfg)
			if err := connectForTLSTest(t, cfg); err == nil {
				t.Fatal("TLS connection unexpectedly succeeded")
			}
			if err := <-serverResult; err == nil {
				t.Fatal("broker handshake unexpectedly succeeded")
			}
		})
	}
}

func TestMutualTLSHandshake(t *testing.T) {
	ca := makeTestCA(t, "mTLS CA")
	serverCert, _, _ := ca.issue(t, "broker.test", []string{"broker.test"}, false)
	_, clientCertPEM, clientKeyPEM := ca.issue(t, "mqtt-raspberry-controller", nil, true)
	clientCAPool := x509.NewCertPool()
	clientCAPool.AppendCertsFromPEM(ca.certPEM)
	address, serverResult := startTLSMQTTBroker(t, &tls.Config{
		Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12,
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientCAPool,
	})
	cfg := tlsClientConfig(t, address, ca)
	cfg.MQTT.AllowUnauthenticated = false
	cfg.MQTT.TLS.CertFile = writeTLSFile(t, "client.pem", clientCertPEM, 0644)
	cfg.MQTT.TLS.KeyFile = writeTLSFile(t, "client.key", clientKeyPEM, 0640)
	if err := connectForTLSTest(t, cfg); err != nil {
		t.Fatalf("mTLS connection failed: %v", err)
	}
	if err := <-serverResult; err != nil {
		t.Fatalf("mTLS broker handshake failed: %v", err)
	}
}

func TestMutualTLSBrokerRejectsMissingClientCertificate(t *testing.T) {
	ca := makeTestCA(t, "mTLS required CA")
	serverCert, _, _ := ca.issue(t, "broker.test", []string{"broker.test"}, false)
	clientCAPool := x509.NewCertPool()
	clientCAPool.AppendCertsFromPEM(ca.certPEM)
	address, serverResult := startTLSMQTTBroker(t, &tls.Config{
		Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12,
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientCAPool,
	})
	cfg := tlsClientConfig(t, address, ca)
	if err := connectForTLSTest(t, cfg); err == nil {
		t.Fatal("mTLS broker accepted a client without a certificate")
	}
	if err := <-serverResult; err == nil {
		t.Fatal("broker handshake unexpectedly succeeded without a client certificate")
	}
}

func TestBrokerLogsAndErrorsDoNotLeakSecrets(t *testing.T) {
	const sentinel = "SENTINEL_DO_NOT_LOG"
	cfg := newTestClientConfig()
	cfg.MQTT.Broker = "ws://127.0.0.1:1/private/" + sentinel
	client, err := New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldWriter); log.SetFlags(oldFlags) })
	_ = client.Connect()
	if strings.Contains(logs.String(), sentinel) {
		t.Fatalf("broker path secret leaked in logs: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "ws://127.0.0.1:1") {
		t.Fatalf("sanitized broker missing from logs: %s", logs.String())
	}

	cfg = newTestClientConfig()
	cfg.MQTT.Broker = "tls://user:" + sentinel + "@broker.test:8883"
	if _, err := New(&cfg); err == nil || strings.Contains(err.Error(), sentinel) {
		t.Fatalf("URI error leaked secret: %v", err)
	}
	cfg = newTestClientConfig()
	cfg.MQTT.AllowUnauthenticated = false
	cfg.MQTT.Username = "device"
	cfg.MQTT.Password = sentinel
	cfg.MQTT.Broker = "tls://broker.test:8883"
	cfg.MQTT.TLS.CertFile = writeTLSFile(t, "bad-cert.pem", []byte(sentinel), 0644)
	cfg.MQTT.TLS.KeyFile = writeTLSFile(t, "bad-key.pem", []byte(sentinel), 0600)
	if _, err := New(&cfg); err == nil || strings.Contains(err.Error(), sentinel) {
		t.Fatalf("certificate error leaked secret: %v", err)
	}
}

func newTestClientConfig() config.Config {
	return config.Config{
		MQTT: config.MQTTConfig{
			Broker: "tcp://127.0.0.1:1883", ClientID: "test", BaseTopic: "test",
			AllowUnauthenticated: true, Discovery: config.DiscoveryConfig{Prefix: config.DefaultDiscoveryPrefix},
		},
		Device:   config.DeviceConfig{ID: "test", Name: "Test"},
		Entities: []config.EntityConfig{{ID: "button", Type: "button", Name: "Button", Press: []config.Action{{Type: "delay", Duration: "1ms"}}}},
	}
}
