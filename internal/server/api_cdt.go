package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/zxcll/vps-panel/internal/alicloud"
	"github.com/zxcll/vps-panel/internal/cdt"
	"github.com/zxcll/vps-panel/internal/store"
)

// --- 视图 ---

// cdtAccountView 是一个账号加上它最新的用量、账单和实例。
type cdtAccountView struct {
	*store.CDTAccount
	// HasCredentials 让前端知道凭据已经填过了，不必回显密文。
	// AccessKeySecret 从不出网。
	HasCredentials bool   `json:"has_credentials"`
	SiteLabel      string `json:"site_label"`
	Cycle          string `json:"cycle"`

	// Usage 是两个额度池的用量与熔断判定。
	Usage cdt.Status `json:"usage"`
	// Regions 是逐地域的明细，用来对照哪个地域吃掉了额度。
	Regions []cdtRegionView `json:"regions"`

	Bill      *store.CDTBill       `json:"bill,omitempty"`
	Instances []*store.CDTInstance `json:"instances"`
	// GuardedCount 是受守护（会被熔断/保活/定时管到）的实例数。
	GuardedCount int `json:"guarded_count"`
}

// cdtRegionView 是一个业务地域的流量，带上它归到哪个额度池。
type cdtRegionView struct {
	BusinessRegionID string `json:"business_region_id"`
	TrafficType      string `json:"traffic_type"`
	TrafficBytes     int64  `json:"traffic_bytes"`
	Bucket           string `json:"bucket"`
	BucketLabel      string `json:"bucket_label"`
}

func cdtSiteLabel(site string) string {
	if site == store.CDTSiteChina {
		return "中国站"
	}
	return "国际站"
}

// --- 请求 ---

type cdtAccountRequest struct {
	Name        string `json:"name"`
	AccessKeyID string `json:"access_key_id"`
	// AccessKeySecret 留空表示不修改已保存的凭据。
	AccessKeySecret string `json:"access_key_secret"`
	RegionID        string `json:"region_id"`
	SiteType        string `json:"site_type"`

	QuotaMainlandGB      float64 `json:"quota_mainland_gb"`
	QuotaOverseasGB      float64 `json:"quota_overseas_gb"`
	ThresholdPercent     float64 `json:"threshold_percent"`
	OutstandingThreshold float64 `json:"outstanding_threshold"`
	ShutdownMode         string  `json:"shutdown_mode"`

	KeepAlive     bool   `json:"keep_alive"`
	AutoStartTime string `json:"auto_start_time"`
	AutoStopTime  string `json:"auto_stop_time"`
	ScheduleTZ    string `json:"schedule_tz"`
	Enabled       bool   `json:"enabled"`
}

// validate 校验并归一化。错误消息直接显示给用户，所以要带上出错的值。
func (req *cdtAccountRequest) validate() error {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return fmt.Errorf("账号名称不能为空")
	}
	req.AccessKeyID = strings.TrimSpace(req.AccessKeyID)
	if req.AccessKeyID == "" {
		return fmt.Errorf("AccessKeyId 不能为空")
	}
	req.AccessKeySecret = strings.TrimSpace(req.AccessKeySecret)

	req.RegionID = strings.TrimSpace(req.RegionID)
	if req.RegionID == "" {
		return fmt.Errorf("请选择地域（ECS 实例在哪个地域就填哪个）")
	}
	if req.SiteType != store.CDTSiteChina {
		req.SiteType = store.CDTSiteInternational
	}
	if req.ShutdownMode != store.CDTStopKeepCharging {
		req.ShutdownMode = store.CDTStopCharging
	}

	// 额度和阈值留 0 交给下游回落默认值 —— cdt.QuotaFromGB 会处理。
	// 但负数一定是填错了，得挡住。
	if req.QuotaMainlandGB < 0 || req.QuotaOverseasGB < 0 {
		return fmt.Errorf("免费额度不能是负数")
	}
	if req.ThresholdPercent < 0 {
		return fmt.Errorf("熔断线 %.1f%% 不合法，应为正数", req.ThresholdPercent)
	}
	if req.OutstandingThreshold < 0 {
		return fmt.Errorf("待还金额熔断线不能是负数（填 0 表示不启用这一条）")
	}

	if err := validateClock(req.AutoStartTime, "定时开机时间"); err != nil {
		return err
	}
	if err := validateClock(req.AutoStopTime, "定时关机时间"); err != nil {
		return err
	}
	req.ScheduleTZ = strings.TrimSpace(req.ScheduleTZ)
	if req.ScheduleTZ == "" {
		req.ScheduleTZ = "Asia/Shanghai"
	}
	if _, err := time.LoadLocation(req.ScheduleTZ); err != nil {
		return fmt.Errorf("时区 %q 不认识", req.ScheduleTZ)
	}
	return nil
}

// validateClock 校验 "HH:MM" 形式的时间。空串表示不启用。
func validateClock(v, field string) error {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	if _, err := time.Parse("15:04", v); err != nil {
		return fmt.Errorf("%s %q 格式不对，应形如 03:30（24 小时制）", field, v)
	}
	return nil
}

func (req *cdtAccountRequest) applyTo(a *store.CDTAccount) {
	a.Name = req.Name
	a.AccessKeyID = req.AccessKeyID
	a.RegionID = req.RegionID
	a.SiteType = req.SiteType
	a.QuotaMainlandGB = req.QuotaMainlandGB
	a.QuotaOverseasGB = req.QuotaOverseasGB
	a.ThresholdPercent = req.ThresholdPercent
	a.OutstandingThreshold = req.OutstandingThreshold
	a.ShutdownMode = req.ShutdownMode
	a.KeepAlive = req.KeepAlive
	a.AutoStartTime = strings.TrimSpace(req.AutoStartTime)
	a.AutoStopTime = strings.TrimSpace(req.AutoStopTime)
	a.ScheduleTZ = req.ScheduleTZ
	a.Enabled = req.Enabled
}

// --- 账号 CRUD ---

func (s *Server) handleListCDTAccounts(w http.ResponseWriter, r *http.Request) {
	views, err := s.cdtViews(r.Context())
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) handleCreateCDTAccount(w http.ResponseWriter, r *http.Request) {
	var req cdtAccountRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.AccessKeySecret == "" {
		writeError(w, http.StatusBadRequest, "请填写 AccessKeySecret")
		return
	}

	ctx := r.Context()
	// 先确认这组凭据真的能用，再落库。在这里失败，总比等到熔断那一刻
	// 才发现凭据无效要好得多 —— 那时候机器该停的没停。
	if err := s.checkCDTCredentials(ctx, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	enc, err := s.cipher.Encrypt(req.AccessKeySecret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "加密凭据失败: "+err.Error())
		return
	}

	a := &store.CDTAccount{}
	req.applyTo(a)
	if err := s.st.CreateCDTAccount(ctx, a, enc); err != nil {
		handleStoreErr(w, err)
		return
	}

	s.st.AddEvent(ctx, nil, store.EventCDTSync, store.LevelInfo,
		fmt.Sprintf("阿里云账号「%s」已添加", a.Name))
	s.syncCDTAccountAsync(ctx, a.ID)

	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) handleUpdateCDTAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req cdtAccountRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	a, err := s.st.GetCDTAccount(ctx, id)
	if err != nil {
		handleStoreErr(w, err)
		return
	}

	// Secret 留空表示不改。要验凭据的话得把库里那份解出来配上新的 AK。
	var enc []byte
	if req.AccessKeySecret != "" {
		if err := s.checkCDTCredentials(ctx, &req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if enc, err = s.cipher.Encrypt(req.AccessKeySecret); err != nil {
			writeError(w, http.StatusInternalServerError, "加密凭据失败: "+err.Error())
			return
		}
	}

	req.applyTo(a)
	if err := s.st.UpdateCDTAccount(ctx, a, enc); err != nil {
		handleStoreErr(w, err)
		return
	}
	s.syncCDTAccountAsync(ctx, a.ID)
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) handleDeleteCDTAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	a, err := s.st.GetCDTAccount(ctx, id)
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	if err := s.st.DeleteCDTAccount(ctx, id); err != nil {
		handleStoreErr(w, err)
		return
	}
	s.st.AddEvent(ctx, nil, store.EventCDTSync, store.LevelInfo,
		fmt.Sprintf("阿里云账号「%s」已删除", a.Name))
	writeJSON(w, http.StatusOK, map[string]string{"message": "已删除"})
}

// handleSyncCDTAccount 是「立即同步」按钮。同步执行，用户点了就该马上看到结果。
func (s *Server) handleSyncCDTAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx, cancel := actionContext(r.Context())
	defer cancel()

	a, err := s.st.GetCDTAccount(ctx, id)
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	if err := s.syncCDTAccount(ctx, a); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	views, err := s.cdtViews(ctx)
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	for _, v := range views {
		if v.ID == id {
			writeJSON(w, http.StatusOK, v)
			return
		}
	}
	writeError(w, http.StatusNotFound, "账号不存在")
}

// handleTestCDTAccount 只验凭据通不通，不改任何东西。
func (s *Server) handleTestCDTAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx, cancel := actionContext(r.Context())
	defer cancel()

	client, err := s.cdtClient(ctx, id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	details, err := client.ListInternetTraffic(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway,
			"凭据不可用："+err.Error()+"（确认 RAM 用户已授予 CDT 只读权限）")
		return
	}
	insts, err := client.DescribeInstances(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway,
			"CDT 通了，但拉不到 ECS 实例："+err.Error()+"（确认已授予 ECS 权限、地域填对了）")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": fmt.Sprintf("凭据可用：拿到 %d 条地域流量明细、%d 台实例", len(details), len(insts)),
	})
}

// --- 实例操作 ---

func (s *Server) handleListCDTInstances(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListCDTInstances(r.Context())
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

type cdtGuardRequest struct {
	Guarded bool `json:"guarded"`
}

func (s *Server) handleGuardCDTInstance(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req cdtGuardRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.st.SetCDTInstanceGuarded(r.Context(), id, req.Guarded); err != nil {
		handleStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"guarded": req.Guarded})
}

func (s *Server) handleStartCDTInstance(w http.ResponseWriter, r *http.Request) {
	s.cdtPower(w, r, true)
}

func (s *Server) handleStopCDTInstance(w http.ResponseWriter, r *http.Request) {
	s.cdtPower(w, r, false)
}

// cdtPower 手动开关机。
//
// 手动开机会顺手解除熔断标记 —— 用户明确要开，就不该让后台循环
// 在下一分钟又把它按回去。反过来，手动关机不落熔断标记：
// 那是「用户自己关的」，不是「面板熔断关的」，两者在月初重置时的
// 处理方式不一样。
func (s *Server) cdtPower(w http.ResponseWriter, r *http.Request, start bool) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx, cancel := actionContext(r.Context())
	defer cancel()

	inst, err := s.st.GetCDTInstance(ctx, id)
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	account, err := s.st.GetCDTAccount(ctx, inst.AccountID)
	if err != nil {
		handleStoreErr(w, err)
		return
	}

	// 同一个账号同时只允许一条动作在跑，避免和后台循环撞车。
	if !s.cdtLock(account.ID) {
		writeError(w, http.StatusConflict, "这个账号上正好有一个操作在执行，请稍后再试")
		return
	}
	defer s.cdtUnlock(account.ID)

	client, err := s.cdtClient(ctx, account.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	action, status := "开机", alicloud.StatusStarting
	if start {
		err = client.StartInstance(ctx, inst.InstanceID)
	} else {
		action, status = "关机", alicloud.StatusStopping
		err = client.StopInstance(ctx, inst.InstanceID, account.ShutdownMode)
	}
	if err != nil {
		msg := fmt.Sprintf("实例「%s」%s失败：%v", instLabel(inst), action, err)
		if alicloud.IsCode(err, alicloud.ErrCodeNoStock) {
			msg += "（这个可用区的抢占式实例已售罄，只能等库存恢复）"
		}
		s.st.AddEvent(ctx, nil, store.EventCDTAction, store.LevelError, msg)
		writeError(w, http.StatusBadGateway, msg)
		return
	}

	s.st.SetCDTInstanceStatus(ctx, inst.ID, status)
	if start && account.Tripped() {
		// 用户明确要开机，解除熔断，别让后台循环下一轮又把它停了。
		s.st.ClearCDTTripped(ctx, account.ID)
	}

	s.st.AddEvent(ctx, nil, store.EventCDTAction, store.LevelWarn,
		fmt.Sprintf("手动对实例「%s」执行了%s", instLabel(inst), action))
	writeJSON(w, http.StatusOK, map[string]string{"message": "已下发" + action + "指令"})
}

func instLabel(inst *store.CDTInstance) string {
	if inst.InstanceName != "" {
		return fmt.Sprintf("%s（%s）", inst.InstanceName, inst.InstanceID)
	}
	return inst.InstanceID
}

// --- 组装视图 ---

func (s *Server) cdtViews(ctx context.Context) ([]*cdtAccountView, error) {
	accounts, err := s.st.ListCDTAccounts(ctx)
	if err != nil {
		return nil, err
	}
	instances, err := s.st.ListCDTInstances(ctx)
	if err != nil {
		return nil, err
	}
	byAccount := map[int64][]*store.CDTInstance{}
	for _, inst := range instances {
		byAccount[inst.AccountID] = append(byAccount[inst.AccountID], inst)
	}

	cycle := string(cdt.CurrentCycle())
	out := make([]*cdtAccountView, 0, len(accounts))
	for _, a := range accounts {
		traffic, err := s.st.CDTTrafficOf(ctx, a.ID, cycle)
		if err != nil {
			return nil, err
		}
		bill, err := s.st.GetCDTBill(ctx, a.ID, cycle)
		if err != nil {
			return nil, err
		}
		enc, err := s.st.CDTAccountCred(ctx, a.ID)
		if err != nil {
			return nil, err
		}

		byRegion := map[string]int64{}
		regions := make([]cdtRegionView, 0, len(traffic))
		for _, d := range traffic {
			byRegion[d.BusinessRegionID] += d.TrafficBytes
			bucket := cdt.ClassifyRegion(d.BusinessRegionID)
			regions = append(regions, cdtRegionView{
				BusinessRegionID: d.BusinessRegionID,
				TrafficType:      d.TrafficType,
				TrafficBytes:     d.TrafficBytes,
				Bucket:           string(bucket),
				BucketLabel:      bucket.Label(),
			})
		}

		insts := byAccount[a.ID]
		if insts == nil {
			insts = []*store.CDTInstance{}
		}
		guarded := 0
		for _, inst := range insts {
			if inst.Guarded {
				guarded++
			}
		}

		out = append(out, &cdtAccountView{
			CDTAccount:     a,
			HasCredentials: len(enc) > 0,
			SiteLabel:      cdtSiteLabel(a.SiteType),
			Cycle:          cycle,
			Usage: cdt.Evaluate(cdt.SumByBucket(byRegion),
				cdt.QuotaFromGB(a.QuotaMainlandGB, a.QuotaOverseasGB), a.ThresholdPercent),
			Regions:      regions,
			Bill:         bill,
			Instances:    insts,
			GuardedCount: guarded,
		})
	}
	return out, nil
}

// --- 小工具 ---

// cdtClient 用库里存的凭据构造一个阿里云客户端。
func (s *Server) cdtClient(ctx context.Context, accountID int64) (*alicloud.Client, error) {
	a, err := s.st.GetCDTAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	enc, err := s.st.CDTAccountCred(ctx, accountID)
	if err != nil {
		return nil, err
	}
	secret, err := s.cipher.Decrypt(enc)
	if err != nil {
		return nil, fmt.Errorf("解密账号「%s」的凭据失败：%w（master.key 换过吗？）", a.Name, err)
	}
	if secret == "" {
		return nil, fmt.Errorf("账号「%s」还没有填写 AccessKeySecret", a.Name)
	}
	return alicloud.New(a.AccessKeyID, secret, a.RegionID, a.SiteType)
}

// checkCDTCredentials 用请求里带的凭据实拨一次，确认它真的能用。
func (s *Server) checkCDTCredentials(ctx context.Context, req *cdtAccountRequest) error {
	client, err := alicloud.New(req.AccessKeyID, req.AccessKeySecret, req.RegionID, req.SiteType)
	if err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	if _, err := client.ListInternetTraffic(probeCtx); err != nil {
		return fmt.Errorf("这组凭据调不通阿里云 CDT 接口：%w"+
			"（确认 RAM 用户已授予 CDT 权限，且 AccessKey 没被禁用）", err)
	}
	return nil
}
