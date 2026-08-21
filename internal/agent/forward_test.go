package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/zxcll/vps-panel/internal/forward"
	"github.com/zxcll/vps-panel/internal/protocol"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestForwarder(t *testing.T, statePath string) *forwarder {
	t.Helper()
	// PoolSize 0 + 不下发用户态规则 = 全程不碰真实端口和 nftables。
	return newForwarder(forwarderConfig{StatePath: statePath, PoolSize: 0, Log: quietLogger()})
}

func TestForwarderGeneratesEpochWhenStateMissing(t *testing.T) {
	f := newTestForwarder(t, filepath.Join(t.TempDir(), "forward.json"))
	if f.epoch == "" {
		t.Fatal("没有状态文件时必须生成一个新 Epoch")
	}
}

// Epoch 在这里扮演 boot_id 的角色：只要状态文件还在，它就不能变，
// 否则面板每次重启探针都会把计数当成"归零了"重新全量计入。
func TestForwarderKeepsEpochAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forward.json")

	f1 := newTestForwarder(t, path)
	first := f1.epoch
	f1.save()

	f2 := newTestForwarder(t, path)
	if f2.epoch != first {
		t.Errorf("状态文件还在时 Epoch 不该变：%q → %q", first, f2.epoch)
	}
}

// 反过来，状态文件没了就意味着逻辑计数从 0 重来，
// 这时候必须换 Epoch，让面板知道该按"归零"处理而不是按"回退"处理。
func TestForwarderRotatesEpochWhenStateLost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "forward.json")

	f1 := newTestForwarder(t, path)
	first := f1.epoch
	f1.save()

	if err := os.Remove(path); err != nil {
		t.Fatalf("删状态文件失败: %v", err)
	}

	f2 := newTestForwarder(t, path)
	if f2.epoch == first {
		t.Error("状态文件丢失后 Epoch 必须换一个新的")
	}
}

func TestForwarderRotatesEpochOnCorruptState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forward.json")
	// 掉电正好卡在写文件中间会留下半个 JSON。读不动就当没有，别拿它去操作防火墙。
	if err := os.WriteFile(path, []byte(`{"version":1,"epoch":"abc`), 0o600); err != nil {
		t.Fatalf("造损坏文件失败: %v", err)
	}
	f := newTestForwarder(t, path)
	if f.epoch == "" || f.epoch == "abc" {
		t.Errorf("损坏的状态文件应触发换 Epoch，实际 %q", f.epoch)
	}
	if len(f.rules) != 0 {
		t.Errorf("损坏的状态文件不该恢复出任何规则，实际 %d 条", len(f.rules))
	}
}

func TestForwarderRotatesEpochOnVersionMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forward.json")
	if err := os.WriteFile(path, []byte(`{"version":99,"epoch":"old","rules":[]}`), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	f := newTestForwarder(t, path)
	if f.epoch == "old" {
		t.Error("版本对不上时应按没有状态处理，换新 Epoch")
	}
}

func TestForwarderRestoresRulesAndCounters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forward.json")

	f1 := newTestForwarder(t, path)
	f1.rules = []forward.Rule{
		{HopID: 7, Proto: forward.ProtoTCP, ListenPort: 8443, DestIP: "1.2.3.4", DestPort: 443},
	}
	f1.rev = "r5"
	f1.dp.SeedCounters(forward.CounterState{
		Logical: map[int64][2]int64{7: {1000, 2000}},
		LastRaw: map[int64][2]int64{7: {1000, 2000}},
	})
	f1.save()

	f2 := newTestForwarder(t, path)
	if len(f2.rules) != 1 || f2.rules[0].HopID != 7 {
		t.Fatalf("规则没恢复出来：%+v", f2.rules)
	}
	if f2.rev != "r5" {
		t.Errorf("规则集版本 = %q，期望 r5", f2.rev)
	}
	got := f2.dp.CounterState()
	if got.Logical[7] != [2]int64{1000, 2000} {
		t.Errorf("计数没恢复出来：%+v", got.Logical)
	}
}

func TestForwarderApplyRejectsInvalidRule(t *testing.T) {
	f := newTestForwarder(t, filepath.Join(t.TempDir(), "forward.json"))
	ack := f.Apply(context.Background(), protocol.ApplyRuleset{
		Rev: "r1",
		Rules: []forward.Rule{
			{HopID: 1, Proto: "sctp", ListenPort: 8443, DestIP: "1.2.3.4", DestPort: 443},
		},
	})
	if ack.OK {
		t.Fatal("非法规则不该被接受")
	}
	if ack.Error == "" {
		t.Error("OK 为 false 时 Error 必须有内容")
	}
	if ack.Rev != "r1" {
		t.Errorf("回执要原样带回 Rev，实际 %q", ack.Rev)
	}
	// 一条不合法就整份拒掉，不能只落地一半 —— 半套防火墙规则比没有更难查。
	if len(f.rules) != 0 {
		t.Errorf("校验失败时不该改动本机规则，实际留下 %d 条", len(f.rules))
	}
}

func TestSamplesCarryEpoch(t *testing.T) {
	f := newTestForwarder(t, filepath.Join(t.TempDir(), "forward.json"))
	f.dp.SeedCounters(forward.CounterState{
		Logical: map[int64][2]int64{7: {1000, 2000}},
		LastRaw: map[int64][2]int64{7: {1000, 2000}},
	})

	samples := f.Samples()
	if len(samples) != 1 {
		t.Fatalf("应有 1 条样本，实际 %d 条", len(samples))
	}
	s := samples[0]
	if s.HopID != 7 || s.BytesUp != 1000 || s.BytesDown != 2000 {
		t.Errorf("样本内容不对：%+v", s)
	}
	if s.Epoch != f.epoch {
		t.Errorf("样本里的 Epoch = %q，期望 %q", s.Epoch, f.epoch)
	}
}

func TestSaveForwardStateIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "forward.json")
	st := forwardState{Version: forwardStateVersion, Epoch: "e1"}
	if err := saveForwardState(path, st); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	// 临时文件必须已经 rename 掉，不能留在目录里。
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("落盘后不该残留临时文件 %s", e.Name())
		}
	}
	got, err := loadForwardState(path)
	if err != nil || got == nil || got.Epoch != "e1" {
		t.Errorf("回读结果不对：%+v, err=%v", got, err)
	}
}
