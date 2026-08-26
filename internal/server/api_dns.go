package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/zxcll/vps-panel/internal/dns"
	"github.com/zxcll/vps-panel/internal/store"
)

// --- 服务商凭据 ---

type providerView struct {
	*store.DNSProvider
	// HasCredentials 让前端知道凭据已填过，不必回显密文
	HasCredentials bool   `json:"has_credentials"`
	TypeLabel      string `json:"type_label"`
}

func providerTypeLabel(t string) string {
	switch t {
	case store.ProviderCloudflare:
		return "Cloudflare"
	case store.ProviderDNSPod:
		return "腾讯云 DNSPod"
	case store.ProviderAlidns:
		return "阿里云 DNS"
	default:
		return t
	}
}

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListDNSProviders(r.Context())
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	out := make([]providerView, 0, len(list))
	for _, p := range list {
		out = append(out, providerView{
			DNSProvider:    p,
			HasCredentials: len(p.CredEnc) > 0,
			TypeLabel:      providerTypeLabel(p.Type),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type providerRequest struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Credentials 为 nil 表示不修改已保存的凭据。
	Credentials *dns.Credentials `json:"credentials"`
}

func (req *providerRequest) validate() error {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return fmt.Errorf("服务商名称不能为空")
	}
	if !dns.ValidType(req.Type) {
		return fmt.Errorf("不支持的服务商类型 %q，目前支持 cloudflare / dnspod / alidns", req.Type)
	}
	return nil
}

// encodeCredentials 校验凭据能构造出可用的客户端，再序列化加密。
// 在这里就失败总比等到切换域名那一刻才失败要好。
func (s *Server) encodeCredentials(typ string, cred *dns.Credentials) ([]byte, error) {
	if cred == nil {
		return nil, nil
	}
	if _, err := dns.New(typ, *cred); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(cred)
	if err != nil {
		return nil, err
	}
	return s.cipher.Encrypt(string(raw))
}

func (s *Server) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
	var req providerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Credentials == nil {
		writeError(w, http.StatusBadRequest, "请填写 API 凭据")
		return
	}

	enc, err := s.encodeCredentials(req.Type, req.Credentials)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	p := &store.DNSProvider{Name: req.Name, Type: req.Type, CredEnc: enc}
	if err := s.st.CreateDNSProvider(r.Context(), p); err != nil {
		handleStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, providerView{
		DNSProvider: p, HasCredentials: true, TypeLabel: providerTypeLabel(p.Type),
	})
}

func (s *Server) handleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req providerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	enc, err := s.encodeCredentials(req.Type, req.Credentials)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.st.UpdateDNSProvider(r.Context(), id, req.Name, req.Type, enc); err != nil {
		handleStoreErr(w, err)
		return
	}
	// 凭据或类型变了，把缓存的客户端丢掉
	s.fo.InvalidateProvider(id)

	writeJSON(w, http.StatusOK, map[string]string{"message": "已保存"})
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.st.DeleteDNSProvider(r.Context(), id); err != nil {
		handleStoreErr(w, err)
		return
	}
	s.fo.InvalidateProvider(id)
	writeJSON(w, http.StatusOK, map[string]string{"message": "已删除"})
}

// --- 记录组 ---

type recordRequest struct {
	ProviderID int64  `json:"provider_id"`
	Zone       string `json:"zone"`
	Name       string `json:"name"`
	RecordType string `json:"record_type"`
	TTL        int    `json:"ttl"`
	Proxied    bool   `json:"proxied"`
	Strategy   string `json:"strategy"`

	SwitchOnExceed  bool `json:"switch_on_exceed"`
	SwitchOnOffline bool `json:"switch_on_offline"`
	SwitchOnWarn    bool `json:"switch_on_warn"`
	// SwitchOnPlannedStop 决定「计划内停机」算不算切换理由。默认关 ——
	// 那是面板通过 CDT 按计划停的机器，不是故障。
	SwitchOnPlannedStop bool `json:"switch_on_planned_stop"`

	Enabled bool               `json:"enabled"`
	Members []recordMemberItem `json:"members"`
}

type recordMemberItem struct {
	NodeID   int64 `json:"node_id"`
	Priority int   `json:"priority"`
}

func (req *recordRequest) validate() error {
	req.Zone = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(req.Zone)), ".")
	req.Name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(req.Name)), ".")

	if req.ProviderID <= 0 {
		return fmt.Errorf("请选择 DNS 服务商")
	}
	if req.Zone == "" {
		return fmt.Errorf("主域名不能为空")
	}
	if req.Name == "" {
		// 只填了主域名时，默认操作根记录
		req.Name = req.Zone
	}
	if req.Name != req.Zone && !strings.HasSuffix(req.Name, "."+req.Zone) {
		return fmt.Errorf("记录名 %q 不属于主域名 %q", req.Name, req.Zone)
	}

	if req.RecordType == "" {
		req.RecordType = "A"
	}
	req.RecordType = strings.ToUpper(req.RecordType)
	if req.RecordType != "A" && req.RecordType != "AAAA" {
		return fmt.Errorf("记录类型只支持 A 或 AAAA")
	}

	if req.TTL <= 0 {
		req.TTL = 60
	}
	if req.Strategy == "" {
		req.Strategy = store.StrategyFailover
	}
	if req.Strategy != store.StrategyFailover && req.Strategy != store.StrategyManual {
		return fmt.Errorf("策略只能是 failover 或 manual")
	}

	seen := map[int64]bool{}
	for _, m := range req.Members {
		if m.NodeID <= 0 {
			return fmt.Errorf("候选节点 ID 不合法")
		}
		if seen[m.NodeID] {
			return fmt.Errorf("候选节点列表里有重复的节点")
		}
		seen[m.NodeID] = true
	}
	if req.Strategy == store.StrategyFailover && len(req.Members) == 0 {
		return fmt.Errorf("自动故障切换需要至少一个候选节点")
	}
	return nil
}

func (req *recordRequest) applyTo(rec *store.DNSRecord) {
	rec.ProviderID = req.ProviderID
	rec.Zone = req.Zone
	rec.Name = req.Name
	rec.RecordType = req.RecordType
	rec.TTL = req.TTL
	rec.Proxied = req.Proxied
	rec.Strategy = req.Strategy
	rec.SwitchOnExceed = req.SwitchOnExceed
	rec.SwitchOnOffline = req.SwitchOnOffline
	rec.SwitchOnWarn = req.SwitchOnWarn
	rec.SwitchOnPlannedStop = req.SwitchOnPlannedStop
	rec.Enabled = req.Enabled
}

func (req *recordRequest) members(recordID int64) []store.DNSMember {
	out := make([]store.DNSMember, 0, len(req.Members))
	for _, m := range req.Members {
		out = append(out, store.DNSMember{RecordID: recordID, NodeID: m.NodeID, Priority: m.Priority})
	}
	return out
}

func (s *Server) handleListRecords(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListDNSRecords(r.Context())
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateRecord(w http.ResponseWriter, r *http.Request) {
	var req recordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	if _, err := s.st.GetDNSProvider(ctx, req.ProviderID); err != nil {
		writeError(w, http.StatusBadRequest, "指定的 DNS 服务商不存在")
		return
	}

	rec := &store.DNSRecord{}
	req.applyTo(rec)
	if err := s.st.CreateDNSRecord(ctx, rec); err != nil {
		handleStoreErr(w, err)
		return
	}
	if err := s.st.ReplaceDNSMembers(ctx, rec.ID, req.members(rec.ID)); err != nil {
		handleStoreErr(w, err)
		return
	}

	full, err := s.st.GetDNSRecord(ctx, rec.ID)
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, full)
}

func (s *Server) handleUpdateRecord(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req recordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	rec, err := s.st.GetDNSRecord(ctx, id)
	if err != nil {
		handleStoreErr(w, err)
		return
	}

	req.applyTo(rec)
	if err := s.st.UpdateDNSRecord(ctx, rec); err != nil {
		handleStoreErr(w, err)
		return
	}
	if err := s.st.ReplaceDNSMembers(ctx, rec.ID, req.members(rec.ID)); err != nil {
		handleStoreErr(w, err)
		return
	}

	full, err := s.st.GetDNSRecord(ctx, rec.ID)
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, full)
}

func (s *Server) handleDeleteRecord(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.st.DeleteDNSRecord(r.Context(), id); err != nil {
		handleStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "已删除"})
}

type switchRequest struct {
	NodeID int64 `json:"node_id"`
}

// handleSwitchRecord 手动一键切换。
func (s *Server) handleSwitchRecord(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req switchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.NodeID <= 0 {
		writeError(w, http.StatusBadRequest, "请指定要切到哪个节点")
		return
	}

	ctx, cancel := actionContext(r.Context())
	defer cancel()

	d, err := s.fo.SwitchTo(ctx, id, req.NodeID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// handleSyncRecord 从服务商拉取记录的当前真实值，写回面板。
// 用于面板和服务商状态对不上时（比如用户在服务商后台直接改过）手动校准。
func (s *Server) handleSyncRecord(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	rec, err := s.st.GetDNSRecord(ctx, id)
	if err != nil {
		handleStoreErr(w, err)
		return
	}

	remote, err := s.fo.FetchCurrent(ctx, rec)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, remote)
}

// handleDNSPlan 是"演练"接口：只算不改，返回每条记录会切到哪、为什么。
// 第一次配置时用它确认逻辑符合预期，再交给自动切换。
func (s *Server) handleDNSPlan(w http.ResponseWriter, r *http.Request) {
	plans, err := s.fo.DryRun(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plans)
}
