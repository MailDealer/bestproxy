package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"

	"github.com/elkin/bestproxy/internal/stats"
)

// classifyErr maps a RoundTrip transport error to a coarse stats.ErrKind for the
// dashboard breakdown. Order matters — more specific / structural checks run before
// string matching. These are the failure modes of a forward/CONNECT tunnel with pooled
// keepalive; there is no per-request retry, so each one surfaced as a 502 to the client.
func classifyErr(err error) stats.ErrKind {
	if err == nil {
		return stats.ErrKindOther
	}

	// Caller went away (client closed the request) — distinct from a deadline.
	if errors.Is(err, context.Canceled) {
		return stats.ErrKindCanceled
	}
	// Any deadline/timeout: context deadline or a net.Error reporting Timeout()
	// (covers dial, TLS-handshake and response-header timeouts).
	if errors.Is(err, context.DeadlineExceeded) {
		return stats.ErrKindTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return stats.ErrKindTimeout
	}

	// Structured TLS failures (cert verification / not-a-TLS-record).
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return stats.ErrKindTLS
	}
	var recErr tls.RecordHeaderError
	if errors.As(err, &recErr) {
		return stats.ErrKindTLS
	}

	msg := err.Error()

	// Stale pooled connection: the peer (origin/CF) closed an idle keepalive conn that
	// we then tried to reuse. Go surfaces this as EOF / reset / broken pipe / an explicit
	// "server closed idle connection". This is the class the shorter IdleConnTimeout targets.
	if errors.Is(err, io.EOF) ||
		strings.Contains(msg, "server closed idle connection") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "unexpected EOF") {
		return stats.ErrKindStale
	}

	// Could not establish the tunnel to the forward proxy at all.
	if strings.Contains(msg, "proxyconnect") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "network is unreachable") ||
		strings.Contains(msg, "no route to host") {
		return stats.ErrKindDial
	}

	// Fallback TLS match for handshake errors not exposed as typed errors.
	if strings.Contains(msg, "tls:") || strings.Contains(msg, "x509:") {
		return stats.ErrKindTLS
	}

	return stats.ErrKindOther
}
