package credentials

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// ErrPasswordUnavailable 区分旧版仅有令牌的登录记录，便于提示重新登录而非重复询问密码。
var ErrPasswordUnavailable = errors.New("实例没有可复用的密码凭据，请重新登录一次")

const passwordPrefix = "enc:v1:"

// passwordCipher 使用用户指定的内置密钥方案；持有二进制者可以提取密钥。
// 版本固定用于跨进程恢复，随机 nonce 与身份绑定防止密文重复或跨账号替换。
func passwordCipher() cipher.AEAD {
	key := sha256.Sum256([]byte("Cosmic-Developers-Union/assistant/credentials/password/v1"))
	block, _ := aes.NewCipher(key[:])
	aead, _ := cipher.NewGCMWithRandomNonce(block)
	return aead
}

func (g Gitea) passwordIdentity() []byte {
	return []byte(NormalizeHost(g.URL) + "\x00" + g.Username)
}

// SetPassword 保存可复用的加密密码，以支持一次登录后为自己或管理员指定用户发令牌。
func (g *Gitea) SetPassword(password string) error {
	if password == "" {
		return fmt.Errorf("密码不能为空")
	}
	sealed := passwordCipher().Seal(nil, nil, []byte(password), g.passwordIdentity())
	g.EncryptedPassword = passwordPrefix + base64.RawStdEncoding.EncodeToString(sealed)
	return nil
}

// PasswordValue 只在需要密码认证时解密，调用方不得输出或记录返回值。
func (g Gitea) PasswordValue() (string, error) {
	if g.EncryptedPassword == "" {
		return "", ErrPasswordUnavailable
	}
	encoded, ok := strings.CutPrefix(g.EncryptedPassword, passwordPrefix)
	if !ok {
		return "", fmt.Errorf("密码凭据格式无效，不接受明文或未知加密版本")
	}
	sealed, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("密码凭据编码无效: %w", err)
	}
	plain, err := passwordCipher().Open(nil, nil, sealed, g.passwordIdentity())
	if err != nil {
		return "", fmt.Errorf("密码凭据损坏或与账号不匹配: %w", err)
	}
	if len(plain) == 0 {
		return "", fmt.Errorf("密码凭据不能为空")
	}
	return string(plain), nil
}
