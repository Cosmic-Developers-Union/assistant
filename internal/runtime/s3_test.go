package runtime

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestS3AdapterProtocolAndFailure(t *testing.T) {
	var put bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "location") {
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<LocationConstraint>us-east-1</LocationConstraint>`)
			return
		}
		if r.Method == "PUT" {
			put = true
			w.Header().Set("ETag", "abc")
			return
		}
		w.Header().Set("Last-Modified", "Mon, 2 Jan 2006 15:04:05 GMT")
		w.Header().Set("Content-Length", "6")
		switch {
		case strings.Contains(r.URL.Path, "missing"):
			w.Header().Del("Content-Length")
			w.WriteHeader(404)
			fmt.Fprint(w, `<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`)
		case strings.Contains(r.URL.Path, "denied"):
			w.Header().Del("Content-Length")
			w.WriteHeader(403)
			fmt.Fprint(w, `<Error><Code>AccessDenied</Code><Message>denied</Message></Error>`)
		default:
			fmt.Fprint(w, "record")
		}
	}))
	defer server.Close()
	bucket, err := NewS3Bucket(SessionConfig{Endpoint: server.URL, Bucket: "sessions", Prefix: "prefix", AccessKey: "access", SecretKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	// 固定 region，避免 SDK 的自动 region 探测扩展测试协议面。
	bucket.Client.SetAppInfo("assistant-test", "1")
	if _, err := NewS3Bucket(SessionConfig{Endpoint: "https://bad/path"}); err == nil {
		t.Fatal("错误端点被接受")
	}
	data, ok, err := bucket.Fetch(t.Context(), "present")
	if err != nil || !ok || string(data) != "record" {
		t.Fatal(string(data), ok, err)
	}
	if _, ok, err := bucket.Fetch(t.Context(), "missing"); err != nil || ok {
		t.Fatal(ok, err)
	}
	if _, _, err := bucket.Fetch(t.Context(), "denied"); err == nil {
		t.Fatal("权限错误被当作缺失")
	}
	if err := bucket.Put(t.Context(), "present", []byte("record")); err != nil || !put {
		t.Fatal(err)
	}
}
