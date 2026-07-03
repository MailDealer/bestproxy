package proxy

import (
	"io"
	"net/http"
	"net/url"
	"strings"
)

// fakeUpstream implements the unexported `upstream` interface for selector/pool tests.
type fakeUpstream struct {
	id      string
	status  Status
	ewma    float64
	backup  bool
	origin  *url.URL
	rt      func(*http.Request) (*http.Response, error)
	reqs    int
	oks     int
	errs    int
	gotBody []byte
}

func newFake(id string, status Status, ewma float64) *fakeUpstream {
	o, _ := url.Parse("https://origin.test")
	return &fakeUpstream{id: id, status: status, ewma: ewma, origin: o}
}

func (f *fakeUpstream) Status() Status      { return f.status }
func (f *fakeUpstream) EWMA() float64       { return f.ewma }
func (f *fakeUpstream) IsBackup() bool      { return f.backup }
func (f *fakeUpstream) Origin() *url.URL    { return f.origin }
func (f *fakeUpstream) RecordRequest()      { f.reqs++ }
func (f *fakeUpstream) RecordSuccess(int64) { f.oks++ }
func (f *fakeUpstream) RecordError()        { f.errs++ }

func (f *fakeUpstream) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		f.gotBody, _ = io.ReadAll(r.Body)
	}
	return f.rt(r)
}

func okResp(code int, body string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
