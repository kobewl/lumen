package tooling

import (
	"io"
	"log/slog"
)

// testLogger 返回一个丢弃输出的日志器，避免测试输出噪声。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
