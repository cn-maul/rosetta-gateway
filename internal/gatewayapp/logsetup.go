package gatewayapp

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// log_format 的三种取值（config.validate 已挡住其它写法）。
const (
	logFormatAuto = "auto"
	logFormatJSON = "json"
	logFormatText = "text"
)

// resolveLogFormat 把「配置值 + stdout 是不是交互终端」换算成真正要用的格式。
//
// 单独抽成纯函数是为了可测：日志格式的判定一旦依赖 os.Stdout 的真假，
// 在测试里就永远只有一种结果，而这里恰恰有三条分支要覆盖。
func resolveLogFormat(configured string, stdoutIsTerminal bool) string {
	switch strings.ToLower(strings.TrimSpace(configured)) {
	case logFormatJSON:
		return logFormatJSON
	case logFormatText:
		return logFormatText
	default:
		// auto / 空串 / 未经校验的怪值：按输出设备判定。
		// 这里**不能**回落到 json —— 空串出现在「读 config 之前」这个
		// 阶段（见 Run 的两阶段装配），那时没人知道运维想要什么，
		// 设备类型是唯一可用的信号。
		if stdoutIsTerminal {
			return logFormatText
		}
		return logFormatJSON
	}
}

// stdoutIsCharDevice 判断 stdout 是否指向字符设备（交互终端/控制台）。
//
// 不用 github.com/mattn/go-isatty：它目前只是 indirect 依赖，而这里只需要
// 「是不是字符设备」这一条信息，os.FileMode 已经够了。重定向到文件或
// 管道时 ModeCharDevice 不置位，正是我们要的「非交互」判据。
func stdoutIsCharDevice(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		// Stat 失败（fd 已关、被接管等）按非交互处理：多写一个 JSON 字段
		// 不会坏，而把 JSON 当文本解析的日志采集器会整个错列。
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// buildLogger 按格式构造写 w 的 slog.Logger。
//
// 两级 level 共用一个 LevelVar：进程生命周期内 log_level 只在启动时设一次，
// 但两阶段装配会建两个 handler，共享同一个 LevelVar 才能保证「换格式不换级别」。
func buildLogger(w io.Writer, level *slog.LevelVar, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if format == logFormatText {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// bufferingHandler 把「logger 装配完成之前」写下的日志先扣在内存里。
//
// 为什么需要：Run 的第一阶段还没有 config，格式只能猜（auto 按输出设备）；
// 读到 cfg.LogFormat 之后会重建 logger。如果第一阶段直接输出，用户看到的就是
// 「首行 JSON、后面全 text」——同一份日志里两种格式，读起来像两段不同程序的
// 输出，而且按行 grep 会漏掉首行。
//
// 它只是过渡态：drain 一次之后就是个空壳（记录已被取走），不在热路径上。
type bufferingHandler struct {
	level  slog.Leveler
	shared *bufferedRecords
	attrs  []slog.Attr
	groups []string
}

// bufferedRecords 是被多个派生 handler 共享的记录槽（WithAttrs/WithGroup
// 每次都会 clone 出新 handler，但记录必须落到同一处）。
type bufferedRecords struct {
	mu      sync.Mutex
	records []bufferedRecord
	taken   bool
}

// bufferedRecord 保留「写它时挂在哪条 attrs/group 链上」，重放时要还原回来。
type bufferedRecord struct {
	attrs  []slog.Attr
	groups []string
	rec    slog.Record
}

func newBufferingHandler(level slog.Leveler) *bufferingHandler {
	return &bufferingHandler{level: level, shared: &bufferedRecords{}}
}

func (h *bufferingHandler) Enabled(_ context.Context, l slog.Level) bool {
	return h.level == nil || l >= h.level.Level()
}

func (h *bufferingHandler) Handle(_ context.Context, r slog.Record) error {
	h.shared.mu.Lock()
	defer h.shared.mu.Unlock()
	h.shared.records = append(h.shared.records, bufferedRecord{
		attrs:  h.attrs,
		groups: h.groups,
		rec:    r.Clone(),
	})
	return nil
}

func (h *bufferingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	next := *h
	next.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &next
}

func (h *bufferingHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	next.groups = append(append([]string{}, h.groups...), name)
	return &next
}

// drain 取走已缓冲的记录（只取一次；之后的写入会被丢弃 —— 那时 logger 早就
// 换成真 handler 了，走到这里说明调用顺序错了）。
func (h *bufferingHandler) drain() []bufferedRecord {
	h.shared.mu.Lock()
	defer h.shared.mu.Unlock()
	recs := h.shared.records
	h.shared.records = nil
	h.shared.taken = true
	return recs
}

// replayRecords 把缓冲的记录按目标 handler 的格式重放一遍。
//
// 必须逐个走 handler.Enabled：第一阶段记录下来的级别可能比配置里的 log_level
// 更啰嗦（配置设成 warn 时，启动那几条 INFO 就不该再冒出来）。
func replayRecords(ctx context.Context, target slog.Handler, recs []bufferedRecord) {
	for _, br := range recs {
		h := target
		for _, g := range br.groups {
			h = h.WithGroup(g)
		}
		if len(br.attrs) > 0 {
			h = h.WithAttrs(br.attrs)
		}
		if !h.Enabled(ctx, br.rec.Level) {
			continue
		}
		_ = h.Handle(ctx, br.rec)
	}
}
