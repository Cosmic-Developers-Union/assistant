package weixin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// QRCode 是扫码登录的二维码信息。
type QRCode struct {
	// Value 是轮询用的二维码值（qrcode）
	Value string
	// Content 是扫码内容（qrcode_img_content，官方客户端用它渲染二维码）
	Content string
}

// Credentials 是扫码登录换到的凭据。
type Credentials struct {
	BotToken string
	BotID    string
	BaseURL  string
	UserID   string
}

// QRStatus 是二维码轮询状态。
type QRStatus struct {
	Status       string
	BotToken     string
	BotID        string
	BaseURL      string
	UserID       string
	RedirectHost string
}

// 状态常量（与服务端返回一致）。
const (
	QRWait            = "wait"
	QRScanned         = "scaned"
	QRConfirmed       = "confirmed"
	QRExpired         = "expired"
	QRNeedVerifyCode  = "need_verifycode"
	QRVerifyBlocked   = "verify_code_blocked"
	QRScannedRedirect = "scaned_but_redirect"
	QRBindedRedirect  = "binded_redirect"
)

// Login 执行扫码登录流程：拉取二维码交给 onQR 展示（用户扫码），轮询状态直到
// confirmed；需要验证码时调 promptVerify 获取（返回空串表示放弃）。
// redirect 状态会切换到服务端返回的地址继续轮询。
func Login(
	ctx context.Context,
	baseURL string,
	onQR func(QRCode),
	promptVerify func(attempt int) (string, error),
	httpClient *http.Client,
) (Credentials, error) {
	client := NewClient(Config{BaseURL: baseURL, HTTPClient: httpClient})
	if onQR == nil {
		onQR = func(QRCode) {}
	}
	if promptVerify == nil {
		promptVerify = func(int) (string, error) { return "", fmt.Errorf("需要输入验证码但当前环境不可交互") }
	}
	verifyAttempt := 0
	refreshCount := 0
	for {
		code, err := fetchQRCode(ctx, client)
		if err != nil {
			return Credentials{}, err
		}
		onQR(code)
		verifyCode := ""
	refresh:
		for {
			status, err := pollQRCode(ctx, client, code.Value, verifyCode)
			if err != nil {
				return Credentials{}, err
			}
			switch status.Status {
			case QRConfirmed:
				credentials := Credentials{
					BotToken: status.BotToken,
					BotID:    status.BotID,
					BaseURL:  firstNonEmpty(status.BaseURL, client.BaseURL()),
					UserID:   status.UserID,
				}
				if credentials.BotToken == "" {
					return Credentials{}, fmt.Errorf("登录成功但响应缺少 bot_token")
				}
				return credentials, nil
			case QRExpired:
				refreshCount++
				if refreshCount > maxQRRefresh {
					return Credentials{}, fmt.Errorf("二维码已连续失效 %d 次，登录中止", maxQRRefresh)
				}
				break refresh
			case QRVerifyBlocked:
				return Credentials{}, fmt.Errorf("验证码错误次数过多，请重新登录")
			case QRBindedRedirect:
				return Credentials{}, fmt.Errorf("该 Bot 已绑定到当前 OpenClaw 实例")
			case QRNeedVerifyCode:
				verifyAttempt++
				entered, err := promptVerify(verifyAttempt)
				if err != nil {
					return Credentials{}, err
				}
				verifyCode = strings.TrimSpace(entered)
				if verifyCode == "" {
					return Credentials{}, fmt.Errorf("未输入验证码，登录中止")
				}
				continue
			case QRScannedRedirect:
				if host := strings.TrimSpace(status.RedirectHost); host != "" {
					client = NewClient(Config{
						BaseURL:    normalizeBaseURL(host),
						RouteTag:   client.config.RouteTag,
						HTTPClient: httpClient,
					})
				}
			}
			select {
			case <-ctx.Done():
				return Credentials{}, ctx.Err()
			case <-time.After(PollInterval):
			}
		}
	}
}

func normalizeBaseURL(host string) string {
	host = strings.TrimSpace(host)
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	return host
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// fetchQRCode 拉取登录二维码（无 Authorization 头）。
func fetchQRCode(ctx context.Context, client *Client) (QRCode, error) {
	body := map[string]any{"local_token_list": []string{}}
	var response struct {
		QRCode  string `json:"qrcode"`
		Content string `json:"qrcode_img_content"`
	}
	if err := client.do(ctx, http.MethodPost, "/ilink/bot/get_bot_qrcode?bot_type=3", body, false, &response); err != nil {
		return QRCode{}, err
	}
	if strings.TrimSpace(response.QRCode) == "" {
		return QRCode{}, fmt.Errorf("get_bot_qrcode 响应缺少 qrcode")
	}
	return QRCode{Value: response.QRCode, Content: response.Content}, nil
}

// pollQRCode 查询一次二维码状态（GET + 应用头，无鉴权；长轮询 35s 超时，
// 超时按 wait 处理继续轮询，与官方客户端一致）。
func pollQRCode(ctx context.Context, client *Client, qrcode, verifyCode string) (QRStatus, error) {
	path := "/ilink/bot/get_qrcode_status?qrcode=" + url.QueryEscape(qrcode)
	if verifyCode != "" {
		path += "&verify_code=" + url.QueryEscape(verifyCode)
	}
	pollCtx, cancel := context.WithTimeout(ctx, qrPollTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(pollCtx, http.MethodGet, client.BaseURL()+path, nil)
	if err != nil {
		return QRStatus{}, err
	}
	client.setHeaders(request, false)
	response, err := client.config.HTTPClient.Do(request)
	if err != nil {
		// 轮询失败按协议建议视为 wait，继续尝试
		return QRStatus{Status: QRWait}, nil
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return QRStatus{Status: QRWait}, nil
	}
	var payload struct {
		Status       string `json:"status"`
		BotToken     string `json:"bot_token"`
		BotID        string `json:"ilink_bot_id"`
		BaseURL      string `json:"baseurl"`
		UserID       string `json:"ilink_user_id"`
		RedirectHost string `json:"redirect_host"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return QRStatus{}, fmt.Errorf("解析二维码状态: %w", err)
	}
	status := QRStatus{
		Status:       strings.TrimSpace(payload.Status),
		BotToken:     payload.BotToken,
		BotID:        payload.BotID,
		BaseURL:      payload.BaseURL,
		UserID:       payload.UserID,
		RedirectHost: payload.RedirectHost,
	}
	if status.Status == "" {
		status.Status = QRWait
	}
	return status, nil
}

// PollInterval 是二维码状态轮询间隔（测试可调小；与服务端长轮询配合，
// 正常响应由服务端在状态变化或超时后返回）。
var PollInterval = time.Second

// qrPollTimeout 是单次状态查询（长轮询）的客户端超时；超时按 wait 继续。
const qrPollTimeout = 35 * time.Second

// maxQRRefresh 是二维码过期后的自动刷新上限。
const maxQRRefresh = 3
