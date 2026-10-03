package runtime

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestObservationsDoNotStoreMemory(t *testing.T) {
	root := t.TempDir()
	observer, _ := NewObserver(root)
	if records, err := observer.Records(); err != nil || len(records) != 0 {
		t.Fatal(err)
	}
	record := Record{Session: "id", Started: time.Now(), Event: Event{Bot: "review", Text: "user message"}, Outcome: Outcome{Result: "agent reply", Errors: []string{"raw error"}, APIError: "raw API error"}}
	if err := observer.Save(record); err != nil {
		t.Fatal(err)
	}
	records, err := observer.Records()
	if err != nil || len(records) != 1 || records[0].Outcome.Result != "" {
		t.Fatal(records, err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "observations"))
	data, _ := os.ReadFile(filepath.Join(root, "observations", entries[0].Name()))
	for _, word := range []string{"user message", "agent reply", "raw error", "raw API error"} {
		if strings.Contains(string(data), word) {
			t.Fatal("复制了记忆", word)
		}
	}
	record.Started = record.Started.Add(time.Second)
	_ = observer.Save(record)
	records, _ = observer.Records()
	if len(records) != 2 || !records[0].Started.Equal(record.Started) {
		t.Fatal("时间线排序不正确")
	}
	_ = os.WriteFile(filepath.Join(root, "observations", "bad.json"), []byte("corrupt"), 0o600)
	if _, err := observer.Records(); err == nil {
		t.Fatal("损坏观测记录被吞掉")
	}
	if _, err := NewObserver(""); err == nil {
		t.Fatal("空根目录被接受")
	}
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, []byte("x"), 0o600)
	bad, _ := NewObserver(file)
	if err := bad.Save(record); err == nil {
		t.Fatal("观测写入失败被吞掉")
	}
	if _, err := bad.Records(); err == nil {
		t.Fatal("观测读取失败被吞掉")
	}
}
func TestStateAPIAuthenticationAndCleanup(t *testing.T) {
	observer, _ := NewObserver(t.TempDir())
	_ = observer.Save(Record{Session: "id", Started: time.Now()})
	server, err := observer.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	endpoint := "http://" + server.Addr().String() + "/state"
	response, err := http.Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatal("匿名读取状态")
	}
	data, _ := os.ReadFile(filepath.Join(observer.Root, "api.json"))
	var descriptor map[string]string
	_ = json.Unmarshal(data, &descriptor)
	info, _ := os.Stat(filepath.Join(observer.Root, "api.json"))
	if info.Mode().Perm() != 0o600 {
		t.Fatal("API 密钥权限错误")
	}
	request, _ := http.NewRequestWithContext(t.Context(), "GET", endpoint, nil)
	request.Header.Set("Authorization", "Bearer "+descriptor["token"])
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !bytes.Contains(body, []byte("id")) {
		t.Fatal(response.StatusCode, string(body))
	}
	if _, err := observer.Listen(server.Addr().String()); err == nil {
		t.Fatal("监听失败被吞掉")
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(observer.Root, "api.json")); !os.IsNotExist(err) {
		t.Fatal("API 密钥残留")
	}
}
func TestSessionsMCP(t *testing.T) {
	observer, _ := NewObserver(t.TempDir())
	_ = observer.Save(Record{Session: "one", Started: time.Now()})
	_ = observer.Save(Record{Session: "two", Started: time.Now()})
	var input strings.Builder
	for i, request := range []string{`"method":"initialize"`, `"method":"tools/list"`, `"method":"ping"`, `"method":"tools/call","params":{"name":"session_list","arguments":{"limit":1}}`, `"method":"tools/call","params":{"name":"session_read","arguments":{"session":"one"}}`, `"method":"tools/call","params":{"name":"session_read"}`, `"method":"tools/call","params":{"name":"unknown"}`, `"method":"unknown"`} {
		fmt.Fprintf(&input, "{\"jsonrpc\":\"2.0\",\"id\":%d,%s}\n", i, request)
	}
	input.WriteString(`{"method":"notifications/initialized"}` + "\n")
	var output bytes.Buffer
	if err := serveSessions(t.Context(), observer, strings.NewReader(input.String()), &output, "test"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 8 {
		t.Fatal(output.String())
	}
	for _, index := range []int{5, 6} {
		if !strings.Contains(lines[index], `"isError":true`) {
			t.Fatal(lines[index])
		}
	}
	if !strings.Contains(lines[7], `-32601`) {
		t.Fatal(lines[7])
	}
	if err := serveSessions(t.Context(), observer, strings.NewReader("bad\n"), &output, ""); err == nil {
		t.Fatal("坏请求被吞掉")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := serveSessions(ctx, observer, strings.NewReader("{}\n"), &output, ""); err == nil {
		t.Fatal("取消未传播")
	}
	t.Setenv("TOKEN", "test-token")
	path := filepath.Join(t.TempDir(), "config.yaml")
	_ = os.WriteFile(path, []byte(validConfig), 0o600)
	if err := ServeSessions(t.Context(), path, strings.NewReader(`{"id":1,"method":"ping"}`+"\n"), &output, "test"); err != nil {
		t.Fatal(err)
	}
	if err := ServeSessions(t.Context(), path+".missing", strings.NewReader(""), &output, ""); err == nil {
		t.Fatal("配置缺失被吞掉")
	}
}

func TestObserverFilesystemAndProtocolFailures(t *testing.T) {
	observer, _ := NewObserver(t.TempDir())
	_ = observer.Save(Record{Session: "../../../escape", Started: time.Now()})
	files, _ := os.ReadDir(filepath.Join(observer.Root, "observations"))
	if len(files) != 1 {
		t.Fatal(files)
	}
	request := "{\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"session_list\",\"arguments\":{\"limit\":1000}}}\n"
	_ = os.Mkdir(filepath.Join(observer.Root, "observations", "skip.json"), 0o700)
	_ = os.WriteFile(filepath.Join(observer.Root, "observations", "skip.txt"), []byte("x"), 0o600)
	var out bytes.Buffer
	if err := serveSessions(t.Context(), observer, strings.NewReader(request), &out, ""); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(observer.Root, "observations", "bad.json"), []byte("bad"), 0o600)
	out.Reset()
	if err := serveSessions(t.Context(), observer, strings.NewReader(request), &out, ""); err != nil || !strings.Contains(out.String(), "isError") {
		t.Fatal(out.String(), err)
	}
	if err := atomicFile(t.TempDir(), []byte("data")); err == nil {
		t.Fatal("观测原子替换失败未报告")
	}
	root := t.TempDir()
	_ = os.Mkdir(filepath.Join(root, "api.json"), 0o700)
	bad, _ := NewObserver(root)
	if _, err := bad.Listen("127.0.0.1:0"); err == nil {
		t.Fatal("API 凭据写入失败未报告")
	}
}

func TestSessionMCPCancelsWhileWaitingForInput(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	reader, writer := io.Pipe()
	defer writer.Close()
	observer, _ := NewObserver(t.TempDir())
	done := make(chan error, 1)
	go func() { done <- serveSessions(ctx, observer, reader, io.Discard, "") }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("等待输入阻塞了取消")
	}
}
