// Package ulid 实现 Lumen 事件协议要求的 ULID 标识符。
//
// ULID 由 48 位毫秒时间戳与 80 位随机数组成，用 Crockford Base32 编码为 26 个字符。
// 相比 UUID v4，它自带时间序，方便按时间排序与排查问题。
//
// 编码细节：16 字节数据是 128 位，26 个 Base32 字符是 130 位，
// 因此需要在最高位补 2 个 0 位后按每 5 位取一个字符。
package ulid

import (
	"crypto/rand"
	"encoding/binary"
	"strings"
	"sync"
	"time"
)

const (
	// Encoding 是 Crockford Base32 字符集，去掉容易混淆的 I、L、O、U。
	Encoding = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	// Length 是 ULID 的字符串长度。
	Length = 26
)

var (
	mu       sync.Mutex
	lastTime int64
	lastRand [10]byte
)

// New 生成当前时刻的 ULID。
func New() string {
	return NewAt(time.Now())
}

// NewAt 生成指定时刻的 ULID。
func NewAt(t time.Time) string {
	mu.Lock()
	defer mu.Unlock()

	ms := t.UnixMilli()
	if ms == lastTime {
		// 同一毫秒内递增随机部分，保证单调不重复。
		incrementRand(&lastRand)
	} else {
		lastTime = ms
		if _, err := rand.Read(lastRand[:]); err != nil {
			// crypto/rand 失败属于系统级异常，用时间戳兜底，保证仍可运行。
			binary.BigEndian.PutUint64(lastRand[:8], uint64(t.UnixNano()))
		}
	}

	var b [16]byte
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	copy(b[6:], lastRand[:])

	return encode(b)
}

// Valid 判断字符串是否是合法 ULID。
func Valid(s string) bool {
	if len(s) != Length {
		return false
	}
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(Encoding, s[i]) < 0 {
			return false
		}
	}
	return true
}

// Time 解析 ULID 中的时间戳，字符串非法时返回零值与 false。
func Time(s string) (time.Time, bool) {
	if !Valid(s) {
		return time.Time{}, false
	}
	// 前 10 个字符承载 50 位：最高 2 位是填充 0，其余 48 位就是毫秒时间戳。
	// 因此这 50 位的数值恰好等于时间戳，无需额外移位。
	var ms int64
	for i := 0; i < 10; i++ {
		ms = ms<<5 | int64(strings.IndexByte(Encoding, s[i]))
	}
	return time.UnixMilli(ms).UTC(), true
}

func incrementRand(r *[10]byte) {
	for i := len(r) - 1; i >= 0; i-- {
		if r[i] == 0xff {
			r[i] = 0
			continue
		}
		r[i]++
		return
	}
	// 全部溢出时重新随机，概率极低。
	_, _ = rand.Read(r[:])
}

// encode 把 128 位数据编码成 26 位 Crockford Base32。
//
// 做法：把 16 字节当作 128 位的位流，前面补 2 个 0 位凑成 130 位，
// 然后每 5 位取一个字符。用一个位缓冲逐步消费，避免对齐错误。
func encode(b [16]byte) string {
	var out [Length]byte

	// 从 -2 开始，表示位流最前面有 2 个填充 0 位；共消费 130 位对应 26 个字符。
	bitPos := -2
	for i := 0; i < Length; i++ {
		var idx uint32
		for k := 0; k < 5; k++ {
			idx <<= 1
			cur := bitPos
			bitPos++
			if cur < 0 {
				// 填充位，值为 0。
				continue
			}
			byteIdx := cur / 8
			bitIdx := 7 - cur%8
			if b[byteIdx]&(1<<uint(bitIdx)) != 0 {
				idx |= 1
			}
		}
		out[i] = Encoding[idx&0x1f]
	}
	return string(out[:])
}
