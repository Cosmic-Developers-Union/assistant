package status

import (
	"io"
	"net/http"
	"time"
)

// retryTransport 对 GET 的瞬时失败（401/408/429/5xx）自动重试。
//
// 动机：Gitea 站点在反向代理/多实例部署下，并发请求里同一有效令牌会被随机
// 判无效（观测于 Gitea 1.27.1 + Caddy：同 token 串行稳定 200、并发随机
// 401/500，assistant 的多通道并发检测正好踩中）。GET 幂等，指数退避重试即可
// 穿越抖动；写操作（POST/PATCH/PUT/DELETE）绝不重试，避免重复提交。
type retryTransport struct {
	base     http.RoundTripper
	attempts int
	backoff  time.Duration
}

func (t *retryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodGet || request.Body != nil {
		return t.base.RoundTrip(request)
	}
	var (
		response *http.Response
		err      error
	)
	for attempt := 0; ; attempt++ {
		response, err = t.base.RoundTrip(request)
		if err != nil || attempt+1 == t.attempts || !transientStatus(response) {
			return response, err
		}
		// 丢弃本次响应体并退避重试；context 取消（服务停机/会话超时）立即放弃
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		timer := time.NewTimer(t.backoff << attempt)
		select {
		case <-request.Context().Done():
			timer.Stop()
			return response, request.Context().Err()
		case <-timer.C:
		}
	}
}

// transientStatus 报告可安全重试的瞬时状态码：401（并发竞态下的令牌误判）、
// 408（请求超时）、429（限流）与服务端 5xx。
func transientStatus(response *http.Response) bool {
	if response == nil {
		return true
	}
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return response.StatusCode >= 500
}
