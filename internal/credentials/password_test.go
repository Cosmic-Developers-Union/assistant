package credentials

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncryptedPasswordRoundTripAndIdentityBinding(t *testing.T) {
	entry := Gitea{Name: "work", URL: "https://gitea.example/", Username: "admin", Token: "token"}
	if _, err := entry.PasswordValue(); !errors.Is(err, ErrPasswordUnavailable) {
		t.Fatal(err)
	}
	if err := entry.SetPassword(""); err == nil {
		t.Fatal("接受空密码")
	}
	if err := entry.SetPassword("password-secret"); err != nil {
		t.Fatal(err)
	}
	first := entry.EncryptedPassword
	if strings.Contains(first, "password-secret") {
		t.Fatal("明文进入密文")
	}
	if err := entry.SetPassword("password-secret"); err != nil {
		t.Fatal(err)
	}
	if entry.EncryptedPassword == first {
		t.Fatal("重复使用 nonce")
	}
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := Save(path, &File{Instances: Instances{Gitea: []Gitea{entry}}}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "password-secret") {
		t.Fatal("明文密码落盘")
	}
	file, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	password, err := file.Instances.Gitea[0].PasswordValue()
	if err != nil || password != "password-secret" {
		t.Fatal(password, err)
	}
	entry.URL = "https://gitea.example"
	entry.Name = "renamed"
	if _, err := entry.PasswordValue(); err != nil {
		t.Fatal("规范化站点或重命名破坏凭据", err)
	}
	for _, target := range []Gitea{
		{URL: "https://other.example", Username: entry.Username, EncryptedPassword: entry.EncryptedPassword},
		{URL: entry.URL, Username: "other", EncryptedPassword: entry.EncryptedPassword},
	} {
		if _, err := target.PasswordValue(); err == nil {
			t.Fatal("密文可以跨账号复制")
		}
	}
}

func TestEncryptedPasswordRejectsCorruptionAndPlaintext(t *testing.T) {
	entry := Gitea{Name: "work", URL: "https://gitea.example", Username: "admin", Token: "token"}
	if err := entry.SetPassword("secret"); err != nil {
		t.Fatal(err)
	}
	data, _ := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(entry.EncryptedPassword, passwordPrefix))
	data[len(data)-1] ^= 1
	empty := passwordCipher().Seal(nil, nil, nil, entry.passwordIdentity())
	for _, invalid := range []string{"plaintext", "enc:v2:data", "enc:v1:!", "enc:v1:", passwordPrefix + base64.RawStdEncoding.EncodeToString(data), passwordPrefix + base64.RawStdEncoding.EncodeToString(empty)} {
		entry.EncryptedPassword = invalid
		if _, err := entry.PasswordValue(); err == nil {
			t.Fatal("接受无效密文", invalid)
		}
		if err := (&File{Instances: Instances{Gitea: []Gitea{entry}}}).Validate(); err == nil {
			t.Fatal("凭据校验忽略无效密文")
		}
	}
}
