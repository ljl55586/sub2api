package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

const debugGatewayBodyDefaultFilename = "gateway_debug.log"

func initDebugGatewayBodyFile(target *atomic.Pointer[os.File], keepAlive **os.File, path string) {
	if parseDebugEnvBool(path) {
		path = debugGatewayBodyDefaultFilename
	}

	if info, err := os.Stat(path); err == nil && info.IsDir() {
		path = filepath.Join(path, debugGatewayBodyDefaultFilename)
	}

	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			slog.Error("failed to create gateway debug log directory", "dir", dir, "error", err)
			return
		}
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		slog.Error("failed to open gateway debug log file", "path", path, "error", err)
		return
	}
	*keepAlive = f
	target.Store(f)
	InitUpstreamAuditLog(path)
	slog.Info("gateway debug logging enabled", "path", path)
}

func debugLogGatewaySnapshot(target *atomic.Pointer[os.File], tag string, headers http.Header, body []byte, extra map[string]string) {
	f := target.Load()
	if f == nil {
		if os.Getenv(debugGatewayBodyEnv) != "" {
			slog.Warn("gateway debug snapshot skipped: file not initialized", "tag", tag)
		}
		return
	}

	var buf strings.Builder
	ts := time.Now().Format("2006-01-02 15:04:05.000")
	fmt.Fprintf(&buf, "\n========== [%s] %s ==========\n", ts, tag)

	if len(extra) > 0 {
		fmt.Fprint(&buf, "--- context ---\n")
		extraKeys := make([]string, 0, len(extra))
		for k := range extra {
			extraKeys = append(extraKeys, k)
		}
		sort.Strings(extraKeys)
		for _, k := range extraKeys {
			fmt.Fprintf(&buf, "  %s: %s\n", k, extra[k])
		}
	}

	fmt.Fprint(&buf, "--- headers ---\n")
	for _, k := range sortHeadersByWireOrder(headers) {
		for _, v := range headers[k] {
			fmt.Fprintf(&buf, "  %s: %s\n", k, safeHeaderValueForLog(k, v))
		}
	}

	fmt.Fprint(&buf, "--- body ---\n")
	if len(body) == 0 {
		fmt.Fprint(&buf, "  (empty)\n")
	} else {
		var pretty bytes.Buffer
		if json.Indent(&pretty, body, "  ", "  ") == nil {
			fmt.Fprintf(&buf, "  %s\n", pretty.Bytes())
		} else {
			fmt.Fprintf(&buf, "  %s\n", body)
		}
	}

	if _, err := f.WriteString(buf.String()); err != nil {
		slog.Warn("gateway debug snapshot write failed", "tag", tag, "error", err)
	}
}

func (s *GatewayService) initDebugGatewayBodyFile(path string) {
	initDebugGatewayBodyFile(&s.debugGatewayBodyFile, &s.debugGatewayBodyFileRef, path)
}

func (s *GatewayService) debugLogGatewaySnapshot(tag string, headers http.Header, body []byte, extra map[string]string) {
	debugLogGatewaySnapshot(&s.debugGatewayBodyFile, tag, headers, body, extra)
}

func (s *OpenAIGatewayService) initDebugGatewayBodyFile(path string) {
	initDebugGatewayBodyFile(&s.debugGatewayBodyFile, &s.debugGatewayBodyFileRef, path)
}

func (s *OpenAIGatewayService) debugLogGatewaySnapshot(tag string, headers http.Header, body []byte, extra map[string]string) {
	debugLogGatewaySnapshot(&s.debugGatewayBodyFile, tag, headers, body, extra)
}
