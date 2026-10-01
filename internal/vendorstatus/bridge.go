package vendorstatus

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Source modules and SVG assets travel with the Go executable. No repository
// checkout, AstrBot installation, or network asset download is needed at runtime.
//
//go:embed worker/statusmonitor/*.py worker/statusmonitor/assets/icons/*
var workerFiles embed.FS

const (
	StateKey       = "monitor_state_v1"
	TranslationKey = "translation_cache_v1"
	maxPacketBytes = 24 << 20
)

type Request struct {
	Operation string                     `json:"operation"`
	Config    Config                     `json:"config"`
	Values    map[string]json.RawMessage `json:"values"`
}

type Packet struct {
	Type  string          `json:"type"`
	Key   string          `json:"key,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
	Group string          `json:"group,omitempty"`
	PNG   []byte          `json:"png,omitempty"`
	Text  string          `json:"text,omitempty"`
	Error string          `json:"error,omitempty"`
}

type Callbacks struct {
	Checkpoint func(string, json.RawMessage) error
	Send       func(context.Context, Packet) error
}

type Runner interface {
	Run(context.Context, Request, Callbacks) (Packet, error)
}

type PythonRunner struct{}

// Run starts a bounded, short-lived worker. It blocks the worker at every
// durable checkpoint and every delivery acknowledgement. A crashed/timed-out
// worker therefore cannot advance past the last committed bbolt checkpoint.
func (PythonRunner) Run(ctx context.Context, request Request, callbacks Callbacks) (Packet, error) {
	if request.Operation != "query" && request.Operation != "cycle" {
		return Packet{}, errors.New("未知厂商状态操作")
	}
	directory, err := os.MkdirTemp("", "new-api-bot-vendor-status-")
	if err != nil {
		return Packet{}, err
	}
	defer os.RemoveAll(directory)
	if err := fs.WalkDir(workerFiles, "worker", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel("worker", path)
		if err != nil {
			return err
		}
		target := filepath.Join(directory, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := workerFiles.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	}); err != nil {
		return Packet{}, err
	}
	processCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(processCtx, request.Config.Python, "-u", "-m", "statusmonitor.worker")
	command.Dir = directory
	command.Env = workerEnvironment(os.Environ())
	command.WaitDelay = 3 * time.Second
	input, err := command.StdinPipe()
	if err != nil {
		return Packet{}, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		return Packet{}, err
	}
	// Bound diagnostics even if a failing dependency produces excessive stderr.
	diagnostics := &limitedBuffer{limit: 8192}
	command.Stderr = diagnostics
	if err := command.Start(); err != nil {
		return Packet{}, fmt.Errorf("启动厂商状态 Python worker 失败（检查 VENDOR_STATUS_PYTHON 和依赖）: %w", err)
	}
	defer input.Close()
	encoder := json.NewEncoder(input)
	if err := encoder.Encode(request); err != nil {
		cancel()
		_ = command.Wait()
		return Packet{}, err
	}
	result, protocolErr := consumePackets(ctx, output, encoder, request.Operation, callbacks)
	if protocolErr != nil {
		cancel()
	}
	waitErr := command.Wait()
	if ctx.Err() != nil {
		return Packet{}, ctx.Err()
	}
	if protocolErr != nil {
		if diagnostics.Len() > 0 {
			return Packet{}, fmt.Errorf("%w; worker stderr: %s", protocolErr, diagnostics.String())
		}
		return Packet{}, protocolErr
	}
	if waitErr != nil {
		return Packet{}, fmt.Errorf("厂商状态 worker 异常退出: %w; %s", waitErr, diagnostics.String())
	}
	return result, nil
}

func consumePackets(ctx context.Context, output io.Reader, encoder *json.Encoder, operation string, callbacks Callbacks) (Packet, error) {
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64<<10), maxPacketBytes)
	var result Packet
	done := false
	for scanner.Scan() {
		if ctx.Err() != nil {
			return Packet{}, ctx.Err()
		}
		if done {
			return Packet{}, errors.New("worker 在完成后继续发送协议消息")
		}
		var packet Packet
		if err := json.Unmarshal(scanner.Bytes(), &packet); err != nil {
			return Packet{}, fmt.Errorf("厂商状态 worker 协议错误: %w", err)
		}
		switch packet.Type {
		case "checkpoint":
			if packet.Key != StateKey && packet.Key != TranslationKey {
				return Packet{}, errors.New("worker 请求写入未知数据键")
			}
			if operation == "query" && packet.Key == StateKey {
				return Packet{}, errors.New("状态查询不得修改告警送达状态")
			}
			if callbacks.Checkpoint == nil {
				return Packet{}, errors.New("缺少厂商状态持久化回调")
			}
			if err := callbacks.Checkpoint(packet.Key, packet.Value); err != nil {
				return Packet{}, fmt.Errorf("保存厂商状态检查点失败: %w", err)
			}
			if err := encoder.Encode(map[string]bool{"ok": true}); err != nil {
				return Packet{}, err
			}
		case "send":
			if operation != "cycle" || callbacks.Send == nil || packet.Group == "" {
				return Packet{}, errors.New("worker 请求了无效的主动投递")
			}
			// Subscription checks are also performed immediately before send by
			// the bot callback, so an unsubscribe during a cycle takes effect.
			sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := callbacks.Send(sendCtx, packet)
			cancel()
			if err := encoder.Encode(map[string]bool{"ok": err == nil}); err != nil {
				return Packet{}, err
			}
		case "done":
			result, done = packet, true
		case "error":
			return Packet{}, fmt.Errorf("厂商状态 worker 失败: %s", packet.Error)
		default:
			return Packet{}, errors.New("未知厂商状态 worker 消息类型")
		}
	}
	if err := scanner.Err(); err != nil {
		return Packet{}, err
	}
	if !done {
		return Packet{}, errors.New("厂商状态 worker 未返回完成结果")
	}
	return result, nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

// Public status collection needs proxy/certificate/runtime settings, not the
// host's QQ, SMTP, New API administrator or database encryption credentials.
func workerEnvironment(environment []string) []string {
	allowed := map[string]bool{
		"PATH": true, "PATHEXT": true, "SYSTEMROOT": true, "WINDIR": true,
		"SYSTEMDRIVE": true, "HOME": true, "USERPROFILE": true,
		"APPDATA": true, "LOCALAPPDATA": true, "TEMP": true, "TMP": true,
		"TMPDIR": true, "LANG": true, "TZ": true, "LD_LIBRARY_PATH": true,
		"DYLD_LIBRARY_PATH": true, "HTTP_PROXY": true, "HTTPS_PROXY": true,
		"ALL_PROXY": true, "NO_PROXY": true, "SSL_CERT_FILE": true,
		"SSL_CERT_DIR": true, "REQUESTS_CA_BUNDLE": true, "CURL_CA_BUNDLE": true,
	}
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		key, _, ok := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		if ok && (allowed[key] || strings.HasPrefix(key, "LC_")) {
			result = append(result, entry)
		}
	}
	return append(result, "PYTHONUTF8=1", "PYTHONDONTWRITEBYTECODE=1")
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	n := len(data)
	if b.Len() < b.limit {
		_, _ = b.Buffer.Write(data[:min(n, b.limit-b.Len())])
	}
	return n, nil
}
