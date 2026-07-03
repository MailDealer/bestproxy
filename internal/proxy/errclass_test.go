package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/elkin/bestproxy/internal/stats"
)

func TestClassifyErr(t *testing.T) {
	timeoutErr := &net.OpError{Op: "dial", Err: &timeoutError{}}

	cases := []struct {
		name string
		err  error
		want stats.ErrKind
	}{
		{"nil", nil, stats.ErrKindOther},
		{"canceled", fmt.Errorf("wrap: %w", context.Canceled), stats.ErrKindCanceled},
		{"deadline", fmt.Errorf("wrap: %w", context.DeadlineExceeded), stats.ErrKindTimeout},
		{"net-timeout", fmt.Errorf("proxy: %w", timeoutErr), stats.ErrKindTimeout},
		{"eof-reuse", fmt.Errorf("read: %w", io.EOF), stats.ErrKindStale},
		{"server-closed-idle", errors.New("http: server closed idle connection"), stats.ErrKindStale},
		{"reset", errors.New("read tcp 1.2.3.4:443: connection reset by peer"), stats.ErrKindStale},
		{"refused", errors.New("proxyconnect tcp: dial tcp 1.2.3.4:443: connect: connection refused"), stats.ErrKindDial},
		{"no-host", errors.New("dial tcp: lookup fwd-x.msndr.net: no such host"), stats.ErrKindDial},
		{"tls-record", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, stats.ErrKindTLS},
		{"tls-string", errors.New("tls: handshake failure"), stats.ErrKindTLS},
		{"x509", errors.New("x509: certificate signed by unknown authority"), stats.ErrKindTLS},
		{"unknown", errors.New("something weird happened"), stats.ErrKindOther},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyErr(tc.err); got != tc.want {
				t.Fatalf("classifyErr(%q) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// timeoutError is a net.Error that reports Timeout() == true.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
