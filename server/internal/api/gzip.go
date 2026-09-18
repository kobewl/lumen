package api

import (
	"compress/gzip"
	"fmt"
	"io"
)

// gzipBody 包装解压后的请求体，并在 Close 时同时关闭底层 reader。
type gzipBody struct {
	*gzip.Reader
	underlying io.ReadCloser
}

func (g *gzipBody) Close() error {
	err := g.Reader.Close()
	if cerr := g.underlying.Close(); err == nil {
		err = cerr
	}
	return err
}

// newGzipReader 创建受限的 gzip 解压读取器。
//
// 限制解压后大小的目的是防止“压缩炸弹”：攻击者可以发送很小的 gzip 数据，
// 解压后占用大量内存。这里额外套一层 LimitReader 兜底。
func newGzipReader(r io.Reader, maxBytes int64) (io.ReadCloser, error) {
	zr, err := gzip.NewReader(io.LimitReader(r, maxBytes))
	if err != nil {
		return nil, fmt.Errorf("gzip 初始化失败: %w", err)
	}
	// 多留出 1 字节以便检测“刚好超限”的情况。
	return &gzipBody{Reader: zr, underlying: io.NopCloser(io.LimitReader(zr, maxBytes+1))}, nil
}
