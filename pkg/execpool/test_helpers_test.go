package execpool

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
)

// stubHandler is a minimal Connect handler for testing the transport layer.
type stubHandler struct {
	executorpbconnect.UnimplementedExecutorServiceHandler
	executorID string
}

func (s *stubHandler) Describe(_ context.Context, req *connect.Request[executorpb.DescribeRequest]) (*connect.Response[executorpb.DescribeResponse], error) {
	return connect.NewResponse(&executorpb.DescribeResponse{
		ExecutorId: s.executorID,
		Tools:      []string{"read", "write", "bash"},
	}), nil
}

func (s *stubHandler) Health(_ context.Context, req *connect.Request[executorpb.HealthRequest]) (*connect.Response[executorpb.HealthResponse], error) {
	return connect.NewResponse(&executorpb.HealthResponse{}), nil
}

func (s *stubHandler) Execute(_ context.Context, req *connect.Request[executorpb.ExecuteRequest], stream *connect.ServerStream[executorpb.ExecuteResponse]) error {
	_ = stream.Send(&executorpb.ExecuteResponse{
		Event: &executorpb.ExecuteResponse_Result{
			Result: &executorpb.Result{Content: []*executorpb.ContentBlock{
				{Block: &executorpb.ContentBlock_Text{Text: "ok"}},
			}},
		},
	})
	return nil
}

func serverTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	cert := testCert(t)
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   ALPNProtocols,
	}
}

func clientTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	return &tls.Config{
		NextProtos:         ALPNProtocols,
		InsecureSkipVerify: true, //nolint:gosec // test-only self-signed cert
	}
}

func testCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
