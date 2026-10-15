package main

// §6.1 的进程级回归测试：令牌布置失败时，网关**不得开始监听**。
//
// # 为什么单测不够
//
// ensureBootstrapToken 返回 error 只钉住了「启动流程里的那个函数知道
// 自己失败了」。真正的承诺是**服务没有进入可接收状态**，而那取决于 main
// 拿到这个 error 之后做了什么 —— 是 os.Exit(1)，还是只记一条日志继续
// 往下走走到 ListenAndServe。后者完全可以发生而不让任何单元测试变红：
// 上一轮的实现正是如此（记 ERROR、return、照常监听）。
//
// 所以这里真的把二进制跑起来，真的去连那个端口。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMainRefusesToServeWhenBootstrapTokenCannotBeEstablished 是本文件的主测试。
//
// 两个用例构成对照，缺一不可：
//
//   - 弱环境变量令牌 → 窗口开着但没保护上 → 进程必须以非零码退出，
//     且**端口从未被接受过连接**。
//   - 合规令牌 → 必须照常监听，否则这条修复就成了「所有部署都起不来」。
//
// 后者尤其重要：fail-secure 的改动最常见的翻车方式就是把正常部署也
// 一起拒掉，而这个 bug 在只测「失败路径」的测试套件里完全隐形。
func TestMainRefusesToServeWhenBootstrapTokenCannotBeEstablished(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the gateway binary; skipped under -short")
	}

	bin := buildGatewayBinary(t)

	t.Run("弱令牌 + 窗口开着 → 拒绝启动，端口从未开放", func(t *testing.T) {
		addr := freeLoopbackAddr(t)
		home := writeTestHome(t, addr)

		// "short" 明确弱于 bootstrapTokenMinLen。
		res := runGateway(t, bin, home, map[string]string{
			bootstrapTokenEnv: "short",
		})

		if res.timedOut {
			t.Fatalf("进程在 %s 内没有退出 —— 它在引导窗口无凭据保护的状态下继续运行了\n%s",
				runGatewayTimeout, res.stdout)
		}
		if res.exitCode == 0 {
			t.Errorf("exit code = 0，进程照常退出 —— 但引导窗口此刻没有任何凭据保护\nstdout:\n%s", res.stdout)
		}
		if served, _ := dialOnce(addr); served {
			t.Error("端口在令牌布置失败后仍然接受连接 —— 这正是 §6.1 描述的 fail-open")
		}
		// 日志必须说清楚「为什么拒绝启动」，而不是只留一句莫名其妙的退出。
		// 运维看到的应该是可据以处置的信息。
		if !strings.Contains(res.stdout, "refusing to start") {
			t.Errorf("缺少拒绝启动的日志，实际输出：\n%s", res.stdout)
		}
	})

	t.Run("合规令牌 → 必须照常监听", func(t *testing.T) {
		addr := freeLoopbackAddr(t)
		home := writeTestHome(t, addr)

		proc := startGatewayProcess(t, bin, home, map[string]string{
			bootstrapTokenEnv: strings.Repeat("a", 64),
		})
		waitForListening(t, addr, proc, 25*time.Second)

		// 窗口确实开着（全新部署），所以免鉴权的 bootstrap 端点可达，
		// 且**要求**令牌 —— 这是「正常启动了但防护也在」的完整确认。
		resp, err := http.Get("http://" + addr + "/admin/api/bootstrap")
		if err != nil {
			t.Fatalf("bootstrap status: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /admin/api/bootstrap = %d, want 200", resp.StatusCode)
		}

		// 光断言 200 不够：那个响应在「窗口关着」时同样是 200。
		// 必须确认 needs_setup 与 requires_token 都为真，才能说这个进程
		// 「窗口开着 **且** 已被令牌保护」。
		var status struct {
			NeedsSetup    bool `json:"needs_setup"`
			RequiresToken bool `json:"requires_token"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
			t.Fatalf("decode bootstrap status: %v", err)
		}
		if !status.NeedsSetup {
			t.Error("needs_setup = false —— 全新部署本应有待初始化的管理员")
		}
		if !status.RequiresToken {
			t.Error("requires_token = false —— 进程监听了，但引导窗口没有任何凭据保护")
		}
	})
}

// buildGatewayBinary 编译一次网关二进制，供本文件所有子测试复用。
//
// 只编译 cmd/gateway（而不是 ./...）：这个测试要的是**能跑的进程**，
// 其余包的构建与断言无关。产物放 t.TempDir，随测试一起清理。
func buildGatewayBinary(t *testing.T) string {
	t.Helper()
	// 后缀用 runtime.GOOS 判断，而不是 os.Getenv("GOOS")：后者在测试
	// 进程里通常是**空的**（GOOS 决定的是编译目标，不是运行期环境变量），
	// 于是 Windows 上也会走到「不加 .exe」那一支，产出一个无法执行的
	// 路径。症状是「executable file not found」—— 与被测逻辑毫无关系，
	// 却很容易被误读成「fail-secure 生效了，端口确实没开」。
	exe := filepath.Join(t.TempDir(), "gateway-under-test")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", exe, ".")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build gateway: %v\n%s", err, out)
	}
	return exe
}

// writeTestHome 写一份「全新部署」的状态目录：空 users 表 + 绑回环的配置。
//
// 绑回环是为了让这个测试对**本机**完全无害（它真的会去监听端口）。
// 注意窗口的暴露面与监听地址无关：引导令牌是窗口的凭据，回环只影响
// 「谁碰得到」，而本测试断言的是「令牌失败时不许开服务」。
func writeTestHome(t *testing.T, listen string) string {
	t.Helper()
	home := t.TempDir()
	cfg := fmt.Sprintf(`{
  "listen": %q,
  "db_path": "gateway.db",
  "log_level": "info"
}
`, listen)
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return home
}

// freeLoopbackAddr 借操作系统分配一个当前空闲的回环端口。
//
// 先 Listen 再 Close 来挑端口有 TOCTOU 窗口（关掉到被别人占之间），
// 但对本地回归测试足够：真撞上了第二个用例会因为「没监听起来」而失败，
// 而不会静默通过或给出误导性的结论。
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	return fmt.Sprintf("127.0.0.1:%d", port)
}

type gatewayRun struct {
	stdout   string
	exitCode int
	// timedOut 为真表示进程在 runGatewayTimeout 内**没有退出**。
	//
	// 这一态就是 fail-open 本身：拒绝启动的修复失效时，网关会照常
	// 跑起来。单独用字段表达（而不是靠 exitCode == 0）是因为它与
	// 「正常退出且码为 0」在断言上要分开看。
	timedOut bool
}

// runGatewayTimeout 是「拒绝启动」这条路径允许的启动耗时上限。
//
// 给得宽（10s）是因为它要覆盖冷启动：打开 SQLite、跑迁移、生成
// master.key 与 session_secret，本机慢盘上这些都是真实的。
const runGatewayTimeout = 10 * time.Second

// runGateway 跑到底（期望它自己退出）并收集日志。
//
// 必须带超时：这条测试断言的正是「**不该**继续跑」，所以「它没退出」
// 是被测行为失效的表现，而不是测试环境的异常。cmd.Output() 会一直
// 等到进程自己结束 —— 缺陷重新出现时（fail-open 回归）本函数会永久
// 挂住，让一个安全回归测试变成 CI 卡死。所以这里自己管超时。
func runGateway(t *testing.T, bin, home string, env map[string]string) gatewayRun {
	t.Helper()
	cmd := gatewayCmd(t, bin, home, env)
	log := &lockedBuffer{}
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatalf("start gateway: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_, _ = cmd.Process.Wait()
	}()

	// Wait 必须放进独立 goroutine：cmd.Process.Wait() 没有超时版本，
	// 直接调会一直阻塞到进程结束 —— 而本测试期望的恰恰是进程**很快**
	// 结束。缺陷回归时（fail-open 回来）它就永远不返回。
	type waitResult struct {
		state *os.ProcessState
		err   error
	}
	done := make(chan waitResult, 1)
	go func() {
		state, err := cmd.Process.Wait()
		done <- waitResult{state: state, err: err}
	}()

	select {
	case r := <-done:
		var ee *exec.ExitError
		res := gatewayRun{stdout: log.String()}
		switch {
		case r.err == nil:
			res.exitCode = r.state.ExitCode()
		case errors.As(r.err, &ee):
			res.exitCode = ee.ExitCode()
		default:
			// 非 ExitError（I/O 层面的失败）没有退出码可言。
			// 用 -1 与真实退出码区分开，与超时的语义一致。
			res.exitCode = -1
		}
		return res
	case <-time.After(runGatewayTimeout):
		// 走到这里说明进程**没有**按预期退出。不在 defer 里静默杀掉，
		// 而是先报出「它还活着」这个事实再返回 —— 那正是本用例要指认的
		// 缺陷。defer 的 Kill 负责收尾，避免留下孤儿进程占着端口。
		return gatewayRun{
			stdout:   log.String(),
			exitCode: -1,
			timedOut: true,
		}
	}
}

// runningGateway 是一个已启动、尚未退出的网关进程及其日志缓冲。
type runningGateway struct {
	cmd *exec.Cmd
	log *lockedBuffer
}

// Logs 返回目前为止进程打印的全部内容。
//
// 日志要在失败时能被拿到：这条测试失败最常见的原因是「配置写错了」
// 或「修复做过头了」，而进程日志是唯一能指认的证据。
func (p *runningGateway) Logs() string { return p.log.String() }

// startGatewayProcess 启动但不等待退出，供「必须能监听」那个用例使用。
//
// 刻意不叫 startGateway：perf_test.go 里已有一个同名 helper（起的是
// httptest.Server，语义完全不同）。同名会让后来者以为两者可以互换，
// 而它们唯一的区别恰好是本测试最要紧的那件事 —— 真的进程 vs 进程内替身。
func startGatewayProcess(t *testing.T, bin, home string, env map[string]string) *runningGateway {
	t.Helper()
	cmd := gatewayCmd(t, bin, home, env)
	log := &lockedBuffer{}
	// 网关所有日志都写 stdout（main.go 用 slog.NewJSONHandler(os.Stdout)）。
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatalf("start gateway: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_, _ = cmd.Process.Wait()
	})
	return &runningGateway{cmd: cmd, log: log}
}

func gatewayCmd(t *testing.T, bin, home string, env map[string]string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin)
	// 二进制的状态根目录：靠它把 config.json / master.key / 数据库指到
	// 本测试的临时目录，绝不碰开发者真实的部署状态。
	cmd.Env = append(os.Environ(),
		homeEnvVar+"="+home,
		// 会话密钥同样落在临时目录里（与 config.json 同根）。
		"TZ=UTC",
	)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return cmd
}

// waitForListening 轮询直到端口接受连接，或进程先死掉。
//
// 进程先死时把它的日志一并带出来：这个测试失败最常见的原因就是
// 「配置写错了」或「上一条修复过头了」，而进程日志是唯一能指认的证据。
func waitForListening(t *testing.T, addr string, proc *runningGateway, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if served, _ := dialOnce(addr); served {
			return
		}
		// Wait 非阻塞：进程已经退了才返回。
		if proc.cmd.ProcessState != nil && proc.cmd.ProcessState.Exited() {
			t.Fatalf("gateway exited (code %d) before it started listening —— "+
				"本用例是要确认「合规令牌下必须能正常启动」。日志：\n%s",
				proc.cmd.ProcessState.ExitCode(), proc.Logs())
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("gateway never started listening。日志：\n%s", proc.Logs())
}

// dialOnce 试一次连接，报告端口是否接受连接。
func dialOnce(addr string) (bool, error) {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false, err
	}
	_ = conn.Close()
	return true, nil
}

// lockedBuffer 是并发安全的日志缓冲。
//
// 必须加锁：waitForListening 在主测试 goroutine 里读它，而 os/exec 的
// 拷贝 goroutine 同时在写。裸 bytes.Buffer 在这种用法下会被 race detector
// 报出来（而 -race 是这个仓库的验证项之一，见 CONTRIBUTING.md）。
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
