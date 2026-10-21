package main

// A10-P1-1（2026-10-21 修）：引导令牌**不得**进普通日志。
//
// # 为什么这条要用测试钉住，而不是靠代码评审
//
// 令牌是「尚未完成引导」这个事实的**唯一持有证明**。它只进日志 =
// 把这个证明复制进了日志系统：日志会轮转、会被聚合到集中式日志平台、
// 甚至会进备份。任何能读这些地方的人都能在引导窗口仍开时抢先设密码 ——
// 而日志本身并不是「保密」的位置。
//
// 旧实现是 logger.Warn("...", "token", token)，一行代码，评审时看着完全
// 无害（「打个日志而已」）。这类缺陷的复发模式恰恰是「后来有人觉得日志
// 不够详细，又加回了 token 字段」—— 只有一条断言能在那一刻把改动挡住。

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// logCapture 把 slog 输出收进一个 buffer，用于断言「日志里没有某个值」。
type logCapture struct {
	buf bytes.Buffer
}

func (c *logCapture) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&c.buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// 随机生成的令牌不得出现在日志里。
func TestEnsureBootstrapToken_令牌不进日志(t *testing.T) {
	t.Setenv(bootstrapTokenEnv, "")
	db := newTestStore(t)
	ctx := context.Background()

	cap := &logCapture{}
	if err := ensureBootstrapToken(ctx, db, cap.logger()); err != nil {
		t.Fatalf("ensureBootstrapToken: %v", err)
	}

	tok, err := db.BootstrapToken(ctx)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	if tok == "" {
		t.Fatal("precondition: 未生成令牌")
	}
	out := cap.buf.String()
	if strings.Contains(out, tok) {
		t.Errorf("引导令牌出现在日志里（凭据被复制进日志系统）：\n%s", out)
	}
	// 反向确认：日志里确实说了「令牌已生成」这件事，
	// 否则这条断言可能因为「日志压根没输出」而空转通过。
	if !strings.Contains(out, "引导令牌") {
		t.Errorf("日志里没有引导令牌的提示（运维无从知道要去数据库取）：\n%s", out)
	}
}

// 环境变量指定的令牌同样不得进日志。
//
// 这一条比上一条更要紧：环境变量里的令牌是**运维自己持有**的值，
// 它从一开始就在运维的部署清单里 —— 把它写进日志等于凭空多出一份副本，
// 而那份副本的保质期是「日志系统的保质期」（可能是永远）。
func TestEnsureBootstrapToken_环境变量令牌不进日志(t *testing.T) {
	const envTok = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Setenv(bootstrapTokenEnv, envTok)
	db := newTestStore(t)
	ctx := context.Background()

	cap := &logCapture{}
	if err := ensureBootstrapToken(ctx, db, cap.logger()); err != nil {
		t.Fatalf("ensureBootstrapToken: %v", err)
	}
	if out := cap.buf.String(); strings.Contains(out, envTok) {
		t.Errorf("环境变量里的引导令牌出现在日志里：\n%s", out)
	}
}

// 「令牌已存在」的那条启动日志也不得回显令牌。
//
// 旧实现在这条分支上只说「不要重复打印」，但它没法自证 ——
// 令牌值是从数据库读出来的，读出来之后顺手打进日志是极自然的写法。
func TestEnsureBootstrapToken_已有令牌时不回显(t *testing.T) {
	t.Setenv(bootstrapTokenEnv, "")
	db := newTestStore(t)
	ctx := context.Background()

	const mine = "aaaaaaaabbbbbbbbccccccccddddddddeeeeeeeeffffffff0000000011111111"
	if err := db.SetBootstrapToken(ctx, mine); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	cap := &logCapture{}
	if err := ensureBootstrapToken(ctx, db, cap.logger()); err != nil {
		t.Fatalf("ensureBootstrapToken: %v", err)
	}
	if out := cap.buf.String(); strings.Contains(out, mine) {
		t.Errorf("重启时的启动日志回显了引导令牌：\n%s", out)
	}
}
