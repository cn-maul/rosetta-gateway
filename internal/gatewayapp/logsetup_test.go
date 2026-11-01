package gatewayapp

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// log_format 的判定表。
//
// 重点在 auto 的两支：容器里 stdout 是管道 → 必须是 json（docs/DOCKER.md 的
// jq 用法、DESIGN §8.4 的「按 JSON 检索聚合」都建立在这条上）；
// 终端里 → text，人能直接读。
func TestResolveLogFormat(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		terminal   bool
		want       string
	}{
		{"auto+终端→text", "auto", true, logFormatText},
		{"auto+管道→json", "auto", false, logFormatJSON},
		{"空串+终端→text", "", true, logFormatText},
		{"空串+管道→json", "", false, logFormatJSON},
		{"显式json+终端仍是json", "json", true, logFormatJSON},
		{"显式json+管道还是json", "json", false, logFormatJSON},
		{"显式text+管道仍是text", "text", false, logFormatText},
		{"显式text+终端是text", "text", true, logFormatText},
		{"大写与空格容忍", "  TEXT ", false, logFormatText},
		{"未经校验的怪值按auto处理", "console", true, logFormatText},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveLogFormat(c.configured, c.terminal); got != c.want {
				t.Fatalf("resolveLogFormat(%q, %v) = %q, want %q", c.configured, c.terminal, got, c.want)
			}
		})
	}
}

// 重定向到文件时不是字符设备 —— 这正是「容器/管道拿 JSON」的判据。
func TestStdoutIsCharDevice_普通文件返回false(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "out.log"))
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer f.Close()
	if stdoutIsCharDevice(f) {
		t.Fatal("普通文件不该被判成字符设备")
	}
}

func TestStdoutIsCharDevice_nil返回false(t *testing.T) {
	if stdoutIsCharDevice(nil) {
		t.Fatal("nil 应判成非交互")
	}
}

// buildLogger 必须真的按格式输出：text 是 key=value，json 是 {...}。
// 这一条钉的是「配置改了到底有没有生效」——只测 resolveLogFormat 的
// 字符串返回值，改坏 buildLogger 的分支也照样绿。
func TestBuildLogger_真的按格式输出(t *testing.T) {
	cases := []struct {
		format string
		prefix string
	}{
		{logFormatText, "time="},
		{logFormatJSON, `{"time":`},
	}
	for _, c := range cases {
		t.Run(c.format, func(t *testing.T) {
			var buf strings.Builder
			lvl := new(slog.LevelVar)
			lvl.Set(slog.LevelInfo)
			buildLogger(&buf, lvl, c.format).Info("hello", "k", "v")
			out := buf.String()
			if !strings.HasPrefix(out, c.prefix) {
				t.Fatalf("%s 格式输出应以 %q 开头，实际：%q", c.format, c.prefix, out)
			}
			if !strings.Contains(out, "hello") || !strings.Contains(out, "k=v") && c.format == logFormatText {
				t.Fatalf("%s 输出缺字段：%q", c.format, out)
			}
		})
	}
}

// 两阶段装配会建两个 handler，必须共用同一个 LevelVar —— 否则「先按设备
// 建一个、读配置后再建一个」时级别会在第二阶段莫名回到 info。
func TestBuildLogger_共享LevelVar(t *testing.T) {
	var buf strings.Builder
	lvl := new(slog.LevelVar)
	lvl.Set(slog.LevelWarn)
	l := buildLogger(&buf, lvl, logFormatText)
	l.Info("不该出现")
	if buf.Len() != 0 {
		t.Fatalf("LevelWarn 下 Info 不该输出：%q", buf.String())
	}
	l.Warn("该出现")
	if !strings.Contains(buf.String(), "该出现") {
		t.Fatalf("Warn 应输出：%q", buf.String())
	}
}

// 第一阶段缓冲的记录必须一条不少地重放进第二阶段，且**整份日志只有一种格式**。
//
// 这一条钉的是「首行 JSON、后面全 text」那个真实出现过的缺陷：
// 只测 resolveLogFormat / buildLogger 都发现不了它，因为缺陷在装配顺序上。
func TestBufferingLogger_重放后格式统一(t *testing.T) {
	var buf strings.Builder
	lvl := new(slog.LevelVar)
	lvl.Set(slog.LevelInfo)

	buffered := newBufferingHandler(lvl)
	first := slog.New(buffered)
	first.Info("会缓冲的一条", "k", "v")
	first.With("with", "attr").Warn("也缓冲")

	// 组装前一个字节都不该落到输出上。
	if buf.Len() != 0 {
		t.Fatalf("重放前不该有输出：%q", buf.String())
	}

	target := buildLogger(&buf, lvl, logFormatText)
	replayRecords(context.Background(), target.Handler(), buffered.drain())

	out := buf.String()
	for _, want := range []string{"会缓冲的一条", "k=v", "也缓冲", "with=attr"} {
		if !strings.Contains(out, want) {
			t.Fatalf("重放后缺 %q：%q", want, out)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.HasPrefix(line, "time=") {
			t.Fatalf("每条都应是 text 格式，这条不是：%q", line)
		}
	}
}

// drain 只能取走一次：正常路径下 defer 的兜底 flush 会再调一次，
// 那次必须是空操作，否则启动日志会被打印两遍。
func TestBufferingLogger_drain只生效一次(t *testing.T) {
	lvl := new(slog.LevelVar)
	lvl.Set(slog.LevelInfo)
	h := newBufferingHandler(lvl)
	slog.New(h).Info("only once")

	if got := len(h.drain()); got != 1 {
		t.Fatalf("首次 drain 应拿到 1 条，实际 %d", got)
	}
	if got := len(h.drain()); got != 0 {
		t.Fatalf("二次 drain 应为空，实际 %d 条 —— 启动日志会被打印两遍", got)
	}
}

// 重放要按目标 handler 的级别过滤：配置把 log_level 设成 warn 时，
// 第一阶段记下的那几条 INFO 不该再冒出来（否则「调了级别却没生效」）。
func TestBufferingLogger_重放遵守目标级别(t *testing.T) {
	var buf strings.Builder
	bufferLevel := new(slog.LevelVar)
	bufferLevel.Set(slog.LevelInfo)
	h := newBufferingHandler(bufferLevel)
	l := slog.New(h)
	l.Info("INFO 不该出现")
	l.Warn("WARN 该出现")

	targetLevel := new(slog.LevelVar)
	targetLevel.Set(slog.LevelWarn)
	replayRecords(context.Background(), buildLogger(&buf, targetLevel, logFormatText).Handler(), h.drain())

	out := buf.String()
	if strings.Contains(out, "INFO 不该出现") {
		t.Fatalf("重放时漏掉了目标级别过滤：%q", out)
	}
	if !strings.Contains(out, "WARN 该出现") {
		t.Fatalf("WARN 没重放出来：%q", out)
	}
}
