package credentials

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRedactErrorPreservesCause(t *testing.T) {
	cause := errors.New("网络故障")
	err := RedactError(fmt.Errorf("https://api.telegram.org/botprivate-token: %w", cause), "private-token", "")
	if !errors.Is(err, cause) || strings.Contains(err.Error(), "private-token") {
		t.Fatal(err)
	}
	if RedactError(nil, "token") != nil {
		t.Fatal("空错误被改变")
	}
}
