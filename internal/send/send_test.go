package send

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSendGzipYAMLAccepted(t *testing.T) {
	var got struct {
		ct, ce, ua, body string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.ct, got.ce, got.ua = r.Header.Get("Content-Type"), r.Header.Get("Content-Encoding"), r.Header.Get("User-Agent")
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("gzip: %v", err)
		}
		b, _ := io.ReadAll(zr)
		got.body = string(b)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	body, err := Gzip([]byte("schema_version: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	res := New(Options{URL: srv.URL, Timeout: time.Second, UserAgent: "argocd-app-chart-preflight/test"}).Send(context.Background(), body)
	if !res.OK() || res.HTTPStatus != 202 || res.Attempts != 1 {
		t.Fatalf("result = %+v", res)
	}
	if got.ct != ContentTypeYAML || got.ce != "gzip" || got.ua != "argocd-app-chart-preflight/test" || got.body != "schema_version: 1\n" {
		t.Errorf("request = %+v", got)
	}
}

func TestSendRetriesOn5xxThenFinal4xxAndRedirectNotFollowed(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&n, 1) {
		case 1:
			w.WriteHeader(http.StatusBadGateway)
		case 2:
			w.WriteHeader(http.StatusAccepted)
		case 3:
			w.WriteHeader(http.StatusUnsupportedMediaType)
		default:
			http.Redirect(w, r, "/elsewhere", http.StatusMovedPermanently)
		}
	}))
	defer srv.Close()
	s := New(Options{URL: srv.URL, Timeout: time.Second, Retries: 2, Backoff: time.Millisecond})
	if res := s.Send(context.Background(), []byte("x")); !res.OK() || res.Attempts != 2 {
		t.Errorf("retry then accept: %+v", res)
	}
	res := s.Send(context.Background(), []byte("x"))
	var se *StatusError
	if res.OK() || !errors.As(res.Err, &se) || se.Code != 415 || res.Attempts != 1 {
		t.Errorf("415 must be final: %+v", res)
	}
	res = s.Send(context.Background(), []byte("x"))
	if res.OK() || res.HTTPStatus != 301 || res.Attempts != 1 {
		t.Errorf("redirect must not be followed: %+v", res)
	}
}

func TestSendTransportErrorHidesURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL + "/v1/preflight?secret=1"
	srv.Close()
	res := New(Options{URL: url, Timeout: time.Second}).Send(context.Background(), []byte("x"))
	if res.OK() || res.HTTPStatus != 0 {
		t.Fatalf("expected transport failure, got %+v", res)
	}
	if strings.Contains(res.Err.Error(), "secret=1") || strings.Contains(res.Err.Error(), "/v1/preflight") {
		t.Errorf("error leaks the URL: %v", res.Err)
	}
}
