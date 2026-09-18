package status

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newRetryClient(attempts int) *http.Client {
	return &http.Client{
		Transport: &retryTransport{
			base:     http.DefaultTransport,
			attempts: attempts,
			backoff:  time.Millisecond,
		},
	}
}

// GET 遇瞬时 401/500 后重试成功：抖动站点（并发下有效令牌被随机判无效）
// 不再中断检测。
func TestRetryTransportRetriesTransientGET(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"invalid username, password or token"}`))
		case 2:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer server.Close()

	response, err := newRetryClient(3).Get(server.URL)
	if err != nil {
		t.Fatalf("GET() error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200（两次瞬时失败后第三次成功）", response.StatusCode)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("calls = %d, want 3", got)
	}
}

// 非瞬时状态码（403/404）与写方法（POST）不重试。
func TestRetryTransportSkipsNonTransientAndWrites(t *testing.T) {
	for _, testcase := range []struct {
		method string
		code   int
		want   int32
	}{
		{http.MethodGet, http.StatusForbidden, 1},
		{http.MethodGet, http.StatusNotFound, 1},
		{http.MethodPost, http.StatusUnauthorized, 1},
	} {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(testcase.code)
		}))
		request, err := http.NewRequestWithContext(context.Background(), testcase.method, server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := newRetryClient(3).Do(request)
		if err != nil {
			t.Fatalf("%s: error = %v", testcase.method, err)
		}
		response.Body.Close()
		if response.StatusCode != testcase.code {
			t.Errorf("%s: status = %d, want %d", testcase.method, response.StatusCode, testcase.code)
		}
		if calls.Load() != testcase.want {
			t.Errorf("%s status %d: calls = %d, want %d（不应重试）",
				testcase.method, testcase.code, calls.Load(), testcase.want)
		}
		server.Close()
	}
}

// 持续 401 时重试到上限后如实返回——真正的令牌错误不会被掩盖，只是多等两次退避。
func TestRetryTransportGivesUpAfterAttempts(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	response, err := newRetryClient(3).Get(server.URL)
	if err != nil {
		t.Fatalf("GET() error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", response.StatusCode)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3（至多尝试 3 次）", calls.Load())
	}
}

// context 取消时立即放弃，不再退避等待。
func TestRetryTransportStopsOnCanceledContext(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &retryTransport{base: http.DefaultTransport, attempts: 5, backoff: 10 * time.Second}
	done := make(chan error, 1)
	go func() {
		response, err := transport.RoundTrip(request)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("取消后未及时返回")
	}
	if calls.Load() > 2 {
		t.Errorf("calls = %d, 取消后不应继续重试", calls.Load())
	}
}
