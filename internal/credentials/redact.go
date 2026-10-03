package credentials

import "strings"

// RedactError 隐去外部错误可能携带的密钥，同时保留错误链供调用方识别。
func RedactError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "***")
		}
	}
	return redactedError{message, err}
}

type redactedError struct {
	message string
	cause   error
}

func (e redactedError) Error() string { return e.message }
func (e redactedError) Unwrap() error { return e.cause }
