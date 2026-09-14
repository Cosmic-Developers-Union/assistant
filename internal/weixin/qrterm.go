package weixin

import (
	"fmt"
	"io"
	"strings"

	"github.com/mdp/qrterminal/v3"
	"rsc.io/qr"
)

// RenderQR 把扫码内容渲染成终端二维码（半块字符，兼容常见终端）。内容为空或
// 渲染失败时返回错误，调用方回退为直接打印内容。
func RenderQR(writer io.Writer, content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return fmt.Errorf("二维码内容为空")
	}
	qrterminal.GenerateWithConfig(content, qrterminal.Config{
		Level:      qr.M,
		Writer:     writer,
		HalfBlocks: true,
	})
	return nil
}
