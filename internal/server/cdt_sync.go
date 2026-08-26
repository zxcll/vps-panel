package server

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zxcll/vps-panel/internal/alicloud"
	"github.com/zxcll/vps-panel/internal/cdt"
	"github.com/zxcll/vps-panel/internal/notify"
	"github.com/zxcll/vps-panel/internal/store"
)

// 调度参数。
//
// 「多久去阿里云查一次」是**每个账号自己配的**（cdt_accounts.sync_interval_sec，
// 默认 300 秒）。流量、账单、实例状态、抢占式实例保活都按那个间隔走 ——
// 它们本来就是同一轮里能一起做完的事，分成好几档只会让「我到底多久查一次」
// 变得说不清。
//
// 只有定时开关机例外，固定一分钟看一次：它是按墙上时钟的 HH:MM 触发的，
// 检查间隔一拉长就会直接错过那个时间点。
const (
	// cdtTickInterval 是调度器自己的心跳。它只负责看看有没有账号到点了，
	// 不直接决定查询频率，所以取一个比最小间隔更细的值。
	cdtTickInterval    = 5 * time.Second
	cdtScheduleEvery   = 1 * time.Minute
	cdtDailyReportHour = 0
	// cdtTransitionPoll 是实例正在开机/关机时的加急间隔。
	//
	// 用户可能把同步间隔设成 600 秒省 API 调用，那点完开关机要干等十分钟
	// 界面才更新，会让人以为没生效又去点一次。状态落定之前先按这个频率盯着。
	cdtTransitionPoll = 15 * time.Second
)

// cdtGuard 保证同一个账号同时只有一条会改变机器状态的动作在跑。
//
// 必要性：后台有四档子任务，加上用户在界面上点开关机，很容易出现
// 「定时关机刚发出去、保活又把它拉起来」这种打架。加锁之后至少
// 同一时刻只有一个操作在动这个账号。
type cdtGuard struct {
	mu      sync.Mutex
	working map[int64]bool

	// lastRun 记每个调度单元上次跑的时间。键是账号 ID 转成的字符串，
	// 或者 "schedule" 这种全局任务名。
	lastRun map[string]time.Time
	// lastReportDay 防止每日汇报在同一天里发第二遍。
	lastReportDay string
}

func newCDTGuard() *cdtGuard {
	return &cdtGuard{working: map[int64]bool{}, lastRun: map[string]time.Time{}}
}

func (s *Server) cdtLock(accountID int64) bool {
	s.cdt.mu.Lock()
	defer s.cdt.mu.Unlock()
	if s.cdt.working[accountID] {
		return false
	}
	s.cdt.working[accountID] = true
	return true
}

func (s *Server) cdtUnlock(accountID int64) {
	s.cdt.mu.Lock()
	delete(s.cdt.working, accountID)
	s.cdt.mu.Unlock()
}

// due 判断某档子任务是不是该跑了，是就顺手记下时间。
func (s *Server) cdtDue(name string, every time.Duration, now time.Time) bool {
	s.cdt.mu.Lock()
	defer s.cdt.mu.Unlock()
	last, ok := s.cdt.lastRun[name]
	if ok && now.Sub(last) < every {
		return false
	}
	s.cdt.lastRun[name] = now
	return true
}

// RunCDTSync 是阿里云 CDT 的后台循环。
func (s *Server) RunCDTSync(ctx context.Context) {
	// 启动时先跑一轮，把面板停机期间积压的账期翻页、熔断恢复补上。
	s.cdtTick(ctx)

	t := time.NewTicker(cdtTickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.cdtTick(ctx)
		}
	}
}

func (s *Server) cdtTick(ctx context.Context) {
	accounts, err := s.st.ListCDTAccounts(ctx)
	if err != nil {
		s.log.Error("读取阿里云账号失败", "err", err)
		return
	}
	if len(accounts) == 0 {
		return
	}

	now := time.Now()
	// 定时开关机是全局一分钟一次的：它按墙上时钟的 HH:MM 触发，
	// 跟着账号的同步间隔走会直接错过时间点。
	scheduleDue := s.cdtDue("schedule", cdtScheduleEvery, now)

	for _, a := range accounts {
		if !a.Enabled {
			continue
		}
		// 账期翻页优先于一切：新的一个月额度重置了，被熔断停掉的机器
		// 该先恢复，再谈别的。这一步只读本地状态，不打阿里云接口，
		// 所以不受同步间隔限制。
		s.cdtRollCycle(ctx, a)

		if scheduleDue {
			s.cdtRunSchedule(ctx, a)
		}

		// 到点了才去查阿里云。间隔由这个账号自己配，但有实例正在开机/关机时
		// 加急盯着，别让用户点完开关机干等一整个周期。
		interval := a.SyncInterval()
		transitioning := s.cdtHasTransitioning(ctx, a.ID)
		if transitioning && interval > cdtTransitionPoll {
			interval = cdtTransitionPoll
		}
		if !s.cdtDue(strconv.FormatInt(a.ID, 10), interval, now) {
			continue
		}

		// 过渡态只刷实例状态就够了 —— 流量和账单那边根本不会因为一次开关机
		// 有什么变化，白白多打两次接口。
		if transitioning {
			s.cdtSyncInstances(ctx, a)
			continue
		}
		s.cdtCheckTraffic(ctx, a)
		s.cdtSyncInstances(ctx, a)
		s.cdtKeepAlive(ctx, a)
	}

	s.cdtDailyReport(ctx, now)
}

// cdtHasTransitioning 判断这个账号下有没有实例正处在开机/关机的中间态。
//
// 只看本地快照，不打阿里云接口 —— 这个判断本身要在每个 tick 上跑，
// 让它去发请求就本末倒置了。
func (s *Server) cdtHasTransitioning(ctx context.Context, accountID int64) bool {
	insts, err := s.st.CDTInstancesOf(ctx, accountID)
	if err != nil {
		return false
	}
	for _, inst := range insts {
		if inst.Status == alicloud.StatusStarting || inst.Status == alicloud.StatusStopping {
			return true
		}
	}
	return false
}

// syncCDTAccount 把一个账号的流量、账单、实例全拉一遍，并做熔断判定。
// 「立即同步」按钮和后台循环都走它。
func (s *Server) syncCDTAccount(ctx context.Context, a *store.CDTAccount) error {
	if err := s.cdtCheckTraffic(ctx, a); err != nil {
		return err
	}
	return s.cdtSyncInstances(ctx, a)
}

// syncCDTAccountAsync 在后台同步一个账号。新建/修改账号后调用，
// 用 WithoutCancel 脱离 HTTP 请求的生命周期，浏览器一关不至于同步到一半就断。
func (s *Server) syncCDTAccountAsync(ctx context.Context, accountID int64) {
	bg := context.WithoutCancel(ctx)
	go func() {
		syncCtx, cancel := context.WithTimeout(bg, 2*time.Minute)
		defer cancel()

		a, err := s.st.GetCDTAccount(syncCtx, accountID)
		if err != nil {
			return
		}
		if err := s.syncCDTAccount(syncCtx, a); err != nil {
			s.log.Warn("阿里云账号同步失败", "账号", a.Name, "err", err)
		}
	}()
}

// --- 流量与熔断 ---

// cdtCheckTraffic 拉一次 CDT 流量和账单，判定要不要熔断。
func (s *Server) cdtCheckTraffic(ctx context.Context, a *store.CDTAccount) error {
	client, err := s.cdtClient(ctx, a.ID)
	if err != nil {
		s.st.MarkCDTSynced(ctx, a.ID, time.Now().UTC(), err.Error())
		return err
	}

	details, err := client.ListInternetTraffic(ctx)
	if err != nil {
		s.st.MarkCDTSynced(ctx, a.ID, time.Now().UTC(), err.Error())
		s.log.Warn("拉取 CDT 流量失败", "账号", a.Name, "err", err)
		return fmt.Errorf("拉取 CDT 流量失败：%w", err)
	}

	cycle := string(cdt.CurrentCycle())
	rows := make([]store.CDTTraffic, 0, len(details))
	byRegion := map[string]int64{}
	for _, d := range details {
		rows = append(rows, store.CDTTraffic{
			BusinessRegionID: d.BusinessRegionID,
			TrafficType:      d.TrafficType,
			TrafficBytes:     d.Traffic,
		})
		byRegion[d.BusinessRegionID] += d.Traffic
	}
	if err := s.st.ReplaceCDTTraffic(ctx, a.ID, cycle, rows); err != nil {
		return fmt.Errorf("保存 CDT 流量失败：%w", err)
	}

	// 账单是可选的：拉不到不该挡住流量熔断这条主路径。
	// 有的 RAM 用户只给了 CDT 和 ECS 权限，没给 BSS。
	outstanding, billErr := s.cdtSyncBill(ctx, a, client, cycle)

	s.st.MarkCDTSynced(ctx, a.ID, time.Now().UTC(), "")

	status := cdt.Evaluate(cdt.SumByBucket(byRegion),
		cdt.QuotaFromGB(a.QuotaMainlandGB, a.QuotaOverseasGB), a.ThresholdPercent)

	reason := status.Reason
	if !status.Trip && a.OutstandingThreshold > 0 && billErr == nil &&
		outstanding >= a.OutstandingThreshold {
		reason = fmt.Sprintf("待还金额 %.2f 已达到熔断线 %.2f", outstanding, a.OutstandingThreshold)
	}
	if reason != "" {
		s.cdtTrip(ctx, a, cycle, reason)
	}
	return nil
}

// cdtSyncBill 拉余额和账单。返回待还总额。
func (s *Server) cdtSyncBill(ctx context.Context, a *store.CDTAccount,
	client *alicloud.Client, cycle string) (float64, error) {

	bill, err := client.QueryBillOverview(ctx, cycle)
	if err != nil {
		s.log.Debug("拉取账单失败（不影响流量熔断）", "账号", a.Name, "err", err)
		return 0, err
	}
	rec := &store.CDTBill{
		AccountID: a.ID, Cycle: cycle,
		Outstanding: bill.TotalOutstanding,
		Currency:    bill.Currency, Symbol: bill.Symbol,
	}
	// 余额单独一个接口，同样是可选的。
	if bal, err := client.QueryAccountBalance(ctx); err == nil {
		rec.AvailableAmount = bal.AvailableAmount
	}
	if err := s.st.SaveCDTBill(ctx, rec); err != nil {
		s.log.Warn("保存账单失败", "账号", a.Name, "err", err)
	}
	return bill.TotalOutstanding, nil
}

// cdtTrip 执行熔断：把这个账号下所有受守护的实例停掉。
//
// 顺序和 engine.handleExceed 一致 —— **先落标记再动机器**。
// 停机不可逆，进程在中途崩了的话，宁可漏执行也不能重复执行。
func (s *Server) cdtTrip(ctx context.Context, a *store.CDTAccount, cycle, reason string) {
	if a.Tripped() {
		return // 已经熔断过了，别重复停。
	}
	if !s.cdtLock(a.ID) {
		return
	}
	defer s.cdtUnlock(a.ID)

	// 重新读一次：可能在拿锁的这段时间里，别的路径已经把它熔断了。
	fresh, err := s.st.GetCDTAccount(ctx, a.ID)
	if err != nil || fresh.Tripped() {
		return
	}

	if err := s.st.MarkCDTTripped(ctx, a.ID, time.Now().UTC(), cycle, reason); err != nil {
		s.log.Error("落熔断标记失败，本轮不执行停机", "账号", a.Name, "err", err)
		return
	}

	msg := fmt.Sprintf("阿里云账号「%s」触发熔断：%s", a.Name, reason)
	s.log.Warn("CDT 熔断", "账号", a.Name, "原因", reason)
	s.st.AddEvent(ctx, nil, store.EventCDTAction, store.LevelError, msg)

	stopped, failed := s.cdtStopGuarded(ctx, a, reason)

	body := msg
	switch {
	case len(stopped) > 0:
		body += "\n已停机：" + strings.Join(stopped, "、")
	case len(failed) == 0:
		body += "\n（这个账号没有标记为「受守护」的实例，只告警不停机）"
	}
	if len(failed) > 0 {
		body += "\n停机失败：" + strings.Join(failed, "；")
	}
	s.st.AddEvent(ctx, nil, store.EventCDTAction, store.LevelWarn, body)
	s.notifier.Send(notify.Message{
		Level: store.LevelError, Title: "阿里云 CDT 熔断", Body: body,
	})
}

// cdtStopGuarded 停掉一个账号下所有受守护、且还在运行的实例。
func (s *Server) cdtStopGuarded(ctx context.Context, a *store.CDTAccount, reason string) (stopped, failed []string) {
	client, err := s.cdtClient(ctx, a.ID)
	if err != nil {
		return nil, []string{err.Error()}
	}
	insts, err := s.st.CDTInstancesOf(ctx, a.ID)
	if err != nil {
		return nil, []string{err.Error()}
	}

	for _, inst := range insts {
		if !inst.Guarded || inst.Status == alicloud.StatusStopped {
			continue
		}
		if err := client.StopInstance(ctx, inst.InstanceID, a.ShutdownMode); err != nil {
			failed = append(failed, fmt.Sprintf("%s：%v", instLabel(inst), err))
			continue
		}
		s.st.SetCDTInstanceStatus(ctx, inst.ID, alicloud.StatusStopping)
		// 关键一步：告诉面板这台机器是**我们自己按计划停的**。
		// 少了它，关联的节点会在几十秒后被判成掉线 —— 发告警、切域名，
		// 把一次计划内的停机当成事故处理。
		s.cdtCtl.MarkNodePlannedStop(ctx, inst.ID, reason)
		stopped = append(stopped, instLabel(inst))
	}
	return stopped, failed
}

// cdtRollCycle 处理账期翻页：新的一个月额度重置了，解除熔断并把机器拉回来。
func (s *Server) cdtRollCycle(ctx context.Context, a *store.CDTAccount) {
	if !a.Tripped() {
		return
	}
	cycle := string(cdt.CurrentCycle())
	if a.TrippedCycle == "" || a.TrippedCycle == cycle {
		return // 还在同一个账期里，熔断继续生效。
	}
	if !s.cdtLock(a.ID) {
		return
	}
	defer s.cdtUnlock(a.ID)

	if err := s.st.ClearCDTTripped(ctx, a.ID); err != nil {
		s.log.Error("解除熔断失败", "账号", a.Name, "err", err)
		return
	}

	msg := fmt.Sprintf("阿里云账号「%s」进入新账期 %s，免费额度已重置，熔断解除", a.Name, cycle)
	s.log.Info("CDT 账期翻页", "账号", a.Name, "账期", cycle)
	s.st.AddEvent(ctx, nil, store.EventCDTAction, store.LevelInfo, msg)

	started := s.cdtStartGuarded(ctx, a)
	if len(started) > 0 {
		msg += "\n已重新启动：" + strings.Join(started, "、")
	}
	s.notifier.Send(notify.Message{
		Level: store.LevelInfo, Title: "阿里云额度已重置", Body: msg,
	})
}

// cdtStartGuarded 拉起一个账号下所有受守护、当前停着的实例。
func (s *Server) cdtStartGuarded(ctx context.Context, a *store.CDTAccount) []string {
	client, err := s.cdtClient(ctx, a.ID)
	if err != nil {
		return nil
	}
	insts, err := s.st.CDTInstancesOf(ctx, a.ID)
	if err != nil {
		return nil
	}

	var started []string
	for _, inst := range insts {
		if !inst.Guarded || inst.Status != alicloud.StatusStopped {
			continue
		}
		if err := client.StartInstance(ctx, inst.InstanceID); err != nil {
			s.log.Warn("账期重置后拉起实例失败", "实例", inst.InstanceID, "err", err)
			continue
		}
		s.st.SetCDTInstanceStatus(ctx, inst.ID, alicloud.StatusStarting)
		// 机器要回来了，把关联节点从「计划内停机」里放出来 ——
		// 置回 unknown，让 engine 顺着心跳自然恢复成 online。
		s.cdtCtl.ClearNodePlannedStop(ctx, inst.ID)
		started = append(started, instLabel(inst))
	}
	return started
}

// --- 实例同步 ---

func (s *Server) cdtSyncInstances(ctx context.Context, a *store.CDTAccount) error {
	client, err := s.cdtClient(ctx, a.ID)
	if err != nil {
		return err
	}
	list, err := client.DescribeInstances(ctx)
	if err != nil {
		s.st.MarkCDTSynced(ctx, a.ID, time.Now().UTC(), err.Error())
		return fmt.Errorf("拉取 ECS 实例失败：%w", err)
	}

	keep := make(map[string]bool, len(list))
	for _, inst := range list {
		keep[inst.InstanceID] = true
		if err := s.st.UpsertCDTInstance(ctx, &store.CDTInstance{
			AccountID:     a.ID,
			InstanceID:    inst.InstanceID,
			InstanceName:  inst.InstanceName,
			RegionID:      inst.RegionID,
			ZoneID:        inst.ZoneID,
			Status:        inst.Status,
			PublicIP:      inst.PublicIP,
			InstanceType:  inst.InstanceType,
			BandwidthMbps: inst.BandwidthMbps,
			IsSpot:        inst.IsSpot,
		}); err != nil {
			return fmt.Errorf("保存实例失败：%w", err)
		}
	}
	// 已经释放掉的实例要清出去，否则它会永远挂在界面上显示成上次的状态。
	if err := s.st.PruneCDTInstances(ctx, a.ID, keep); err != nil {
		s.log.Warn("清理已释放实例失败", "账号", a.Name, "err", err)
	}
	return nil
}

// --- 保活 ---

// cdtKeepAlive 把被回收的抢占式实例重新拉起来。
//
// 只对开了 keep_alive、且没有处于熔断状态的账号做。熔断状态下机器是
// 「面板故意停的」，保活再把它拉起来就成了自己和自己打架。
func (s *Server) cdtKeepAlive(ctx context.Context, a *store.CDTAccount) {
	if !a.KeepAlive || a.Tripped() {
		return
	}
	insts, err := s.st.CDTInstancesOf(ctx, a.ID)
	if err != nil {
		return
	}

	// 先看有没有活要干，没有就别去拿锁、更别去调阿里云。
	var targets []*store.CDTInstance
	for _, inst := range insts {
		if inst.Guarded && inst.IsSpot {
			targets = append(targets, inst)
		}
	}
	if len(targets) == 0 {
		return
	}
	if !s.cdtLock(a.ID) {
		return
	}
	defer s.cdtUnlock(a.ID)

	client, err := s.cdtClient(ctx, a.ID)
	if err != nil {
		return
	}

	for _, inst := range targets {
		status, err := client.DescribeInstanceStatus(ctx, inst.InstanceID)
		if err != nil {
			s.log.Debug("查实例状态失败", "实例", inst.InstanceID, "err", err)
			continue
		}
		s.st.SetCDTInstanceStatus(ctx, inst.ID, status)
		if status != alicloud.StatusStopped {
			continue
		}

		if err := client.StartInstance(ctx, inst.InstanceID); err != nil {
			s.cdtHandleNoStock(ctx, a, inst, err)
			continue
		}
		s.st.SetCDTInstanceStatus(ctx, inst.ID, alicloud.StatusStarting)
		s.cdtCtl.ClearNodePlannedStop(ctx, inst.ID)

		// 之前报过库存不足，这次成功了，说明库存回来了，值得说一声。
		if a.NoStockNotified {
			s.st.SetCDTNoStockNotified(ctx, a.ID, false)
			s.notifier.Send(notify.Message{
				Level: store.LevelInfo, Title: "抢占式实例库存已恢复",
				Body: fmt.Sprintf("账号「%s」的实例「%s」已重新启动", a.Name, instLabel(inst)),
			})
		}

		msg := fmt.Sprintf("实例「%s」被回收后已由保活自动拉起", instLabel(inst))
		s.log.Info("保活拉起实例", "账号", a.Name, "实例", inst.InstanceID)
		s.st.AddEvent(ctx, nil, store.EventCDTAction, store.LevelWarn, msg)
		s.notifier.Send(notify.Message{
			Level: store.LevelWarn, Title: "抢占式实例已自动拉起",
			Body: fmt.Sprintf("账号「%s」：%s", a.Name, msg),
		})
	}
}

// cdtHandleNoStock 处理「可用区售罄」。
//
// 售罄是常态，保活每一轮都会试，不能每次都发通知 —— 一晚上能刷几百条。
// 所以只在第一次报，恢复时再报一次。
func (s *Server) cdtHandleNoStock(ctx context.Context, a *store.CDTAccount,
	inst *store.CDTInstance, err error) {

	if !alicloud.IsCode(err, alicloud.ErrCodeNoStock) {
		s.log.Warn("保活拉起实例失败", "实例", inst.InstanceID, "err", err)
		s.st.AddEvent(ctx, nil, store.EventCDTAction, store.LevelError,
			fmt.Sprintf("保活拉起实例「%s」失败：%v", instLabel(inst), err))
		return
	}
	if a.NoStockNotified {
		return // 已经报过了，静默重试。
	}
	s.st.SetCDTNoStockNotified(ctx, a.ID, true)

	msg := fmt.Sprintf("账号「%s」的实例「%s」所在可用区抢占式实例已售罄，"+
		"面板会持续重试，库存恢复后自动拉起。", a.Name, instLabel(inst))
	s.log.Warn("抢占式实例库存不足", "账号", a.Name, "实例", inst.InstanceID)
	s.st.AddEvent(ctx, nil, store.EventCDTAction, store.LevelWarn, msg)
	s.notifier.Send(notify.Message{
		Level: store.LevelWarn, Title: "抢占式实例库存不足", Body: msg,
	})
}

// --- 定时开关机 ---

// cdtRunSchedule 到点了就开关机。
//
// 这个循环一分钟跑一次，判定的是「当前时刻的 HH:MM 是否等于设定值」。
// 因此面板停机超过一分钟会错过那个点 —— 这是有意的取舍：补执行一个
// 半小时前的关机指令，比错过它更让人意外。
func (s *Server) cdtRunSchedule(ctx context.Context, a *store.CDTAccount) {
	if a.AutoStartTime == "" && a.AutoStopTime == "" {
		return
	}
	loc, err := time.LoadLocation(a.ScheduleTZ)
	if err != nil {
		loc = time.UTC
	}
	nowHM := time.Now().In(loc).Format("15:04")

	switch nowHM {
	case a.AutoStopTime:
		s.cdtScheduledPower(ctx, a, false)
	case a.AutoStartTime:
		s.cdtScheduledPower(ctx, a, true)
	}
}

func (s *Server) cdtScheduledPower(ctx context.Context, a *store.CDTAccount, start bool) {
	if !s.cdtLock(a.ID) {
		return
	}
	defer s.cdtUnlock(a.ID)

	var names []string
	action := "定时关机"
	if start {
		action = "定时开机"
		// 定时开机顺带解除熔断：用户既然安排了每天这个点开机，
		// 就不该让上个月的熔断标记一直把它按在那儿。
		if a.Tripped() {
			s.st.ClearCDTTripped(ctx, a.ID)
		}
		names = s.cdtStartGuarded(ctx, a)
	} else {
		stopped, failed := s.cdtStopGuarded(ctx, a, action)
		names = stopped
		if len(failed) > 0 {
			s.st.AddEvent(ctx, nil, store.EventCDTAction, store.LevelError,
				fmt.Sprintf("账号「%s」%s部分失败：%s", a.Name, action, strings.Join(failed, "；")))
		}
	}

	if len(names) == 0 {
		return
	}
	msg := fmt.Sprintf("账号「%s」执行%s：%s", a.Name, action, strings.Join(names, "、"))
	s.log.Info("CDT 定时任务", "账号", a.Name, "动作", action)
	s.st.AddEvent(ctx, nil, store.EventCDTAction, store.LevelWarn, msg)
	s.notifier.Send(notify.Message{
		Level: store.LevelInfo, Title: action, Body: msg,
	})
}

// --- 每日汇报 ---

// cdtDailyReport 每天零点推一份各账号的用量汇总。
func (s *Server) cdtDailyReport(ctx context.Context, now time.Time) {
	if now.Hour() != cdtDailyReportHour {
		return
	}
	day := now.Format("2006-01-02")

	s.cdt.mu.Lock()
	if s.cdt.lastReportDay == day {
		s.cdt.mu.Unlock()
		return
	}
	s.cdt.lastReportDay = day
	s.cdt.mu.Unlock()

	views, err := s.cdtViews(ctx)
	if err != nil || len(views) == 0 {
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "阿里云 CDT 用量汇报（账期 %s）\n", views[0].Cycle)
	for _, v := range views {
		if !v.Enabled {
			continue
		}
		fmt.Fprintf(&b, "\n【%s】%s\n", v.Name, v.SiteLabel)
		for _, u := range v.Usage.Buckets {
			fmt.Fprintf(&b, "  %s：%s / %s（%.1f%%）\n",
				u.Label, cdt.HumanBytes(u.Used), cdt.HumanBytes(u.Quota), u.Percent)
		}
		if v.Bill != nil {
			fmt.Fprintf(&b, "  余额 %s%.2f　待还 %s%.2f\n",
				v.Bill.Symbol, v.Bill.AvailableAmount, v.Bill.Symbol, v.Bill.Outstanding)
		}
		running := 0
		for _, inst := range v.Instances {
			if inst.Status == alicloud.StatusRunning {
				running++
			}
		}
		fmt.Fprintf(&b, "  实例 %d 台（运行中 %d，受守护 %d）\n",
			len(v.Instances), running, v.GuardedCount)
		if v.Tripped() {
			fmt.Fprintf(&b, "  ⚠ 已熔断：%s\n", v.TrippedReason)
		}
		if v.NoStockNotified {
			fmt.Fprintf(&b, "  ⚠ 抢占式实例库存不足，保活持续重试中\n")
		}
	}

	s.notifier.Send(notify.Message{
		Level: store.LevelInfo, Title: "阿里云 CDT 每日汇报", Body: b.String(),
	})
	s.st.AddEvent(ctx, nil, store.EventCDTSync, store.LevelInfo, "已发送阿里云 CDT 每日汇报")
}
