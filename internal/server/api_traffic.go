package server

import (
	"net/http"
	"time"

	"github.com/zxcll/vps-panel/internal/store"
)

// handleNodeTraffic 返回流量曲线数据。
// range 支持 24h / 7d / 30d / cycle（当前账单周期）。
func (s *Server) handleNodeTraffic(w http.ResponseWriter, r *http.Request) {
	n, ok := s.mustNode(w, r)
	if !ok {
		return
	}

	now := time.Now().UTC()
	rng := r.URL.Query().Get("range")

	var from time.Time
	var bucket string
	switch rng {
	case "7d":
		from, bucket = now.AddDate(0, 0, -7), "hour"
	case "30d":
		from, bucket = now.AddDate(0, 0, -30), "day"
	case "cycle":
		from, bucket = n.CycleStart, "day"
		if from.IsZero() {
			from = now.AddDate(0, 0, -30)
		}
	default: // 24h
		from, bucket = now.Add(-24*time.Hour), "hour"
	}

	from = from.Truncate(time.Hour)
	points, err := s.st.HourlyRange(r.Context(), n.ID, from, now)
	if err != nil {
		handleStoreErr(w, err)
		return
	}

	// 补齐没有流量的小时。库里只存有数据的桶，直接画出来时间轴就不是等距的——
	// 三小时的空档看起来和一小时一样，那是在骗人。
	points = densifyHourly(points, from, now)

	if bucket == "day" {
		points = aggregateDaily(points, n.ResetTZ)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"node_id": n.ID,
		"range":   rng,
		"bucket":  bucket,
		"from":    from,
		"to":      now,
		"points":  points,
	})
}

// densifyHourly 把稀疏的小时数据补成 [from, to] 之间每小时一条的连续序列，
// 缺失的小时补零。
func densifyHourly(points []store.HourlyPoint, from, to time.Time) []store.HourlyPoint {
	from = from.UTC().Truncate(time.Hour)
	to = to.UTC().Truncate(time.Hour)
	if !from.Before(to) && !from.Equal(to) {
		return points
	}

	byHour := make(map[int64]store.HourlyPoint, len(points))
	for _, p := range points {
		byHour[p.HourTS.Unix()] = p
	}

	// 上限兜底，防止有人传个荒唐的时间范围把内存吃光
	const maxBuckets = 24 * 400
	out := make([]store.HourlyPoint, 0, len(points)+24)
	for t, i := from, 0; !t.After(to) && i < maxBuckets; t, i = t.Add(time.Hour), i+1 {
		if p, ok := byHour[t.Unix()]; ok {
			out = append(out, p)
			continue
		}
		out = append(out, store.HourlyPoint{HourTS: t})
	}
	return out
}

// aggregateDaily 把小时数据合并成天。
// 按节点自己的时区分天，这样"某一天用了多少"和商家账单对得上。
func aggregateDaily(points []store.HourlyPoint, tz string) []store.HourlyPoint {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}

	var out []store.HourlyPoint
	var cur store.HourlyPoint
	var curDay time.Time
	started := false

	for _, p := range points {
		local := p.HourTS.In(loc)
		day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)

		if !started {
			curDay, cur = day, store.HourlyPoint{HourTS: day.UTC()}
			started = true
		} else if !day.Equal(curDay) {
			out = append(out, cur)
			curDay, cur = day, store.HourlyPoint{HourTS: day.UTC()}
		}
		cur.RxDelta += p.RxDelta
		cur.TxDelta += p.TxDelta
	}
	if started {
		out = append(out, cur)
	}
	if out == nil {
		out = []store.HourlyPoint{}
	}
	return out
}

func (s *Server) handleNodeCycles(w http.ResponseWriter, r *http.Request) {
	n, ok := s.mustNode(w, r)
	if !ok {
		return
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), 24)
	cycles, err := s.st.ListCycles(r.Context(), n.ID, limit)
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cycles)
}

// overviewResponse 是总览页一次拉回的全部数据。
type overviewResponse struct {
	Nodes    []*nodeView    `json:"nodes"`
	Summary  overviewStats  `json:"summary"`
	Events   []*store.Event `json:"recent_events"`
	ServerAt time.Time      `json:"server_time"`
}

type overviewStats struct {
	Total      int   `json:"total"`
	Online     int   `json:"online"`
	Offline    int   `json:"offline"`
	Exceeded   int   `json:"exceeded"`
	Warning    int   `json:"warning"`
	TotalRx    int64 `json:"total_rx"`
	TotalTx    int64 `json:"total_tx"`
	TotalRate  int64 `json:"total_rate"` // 当前总带宽（字节/秒，上下行相加）
	TotalQuota int64 `json:"total_quota"`
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	nodes, err := s.st.ListNodes(ctx)
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	usages, err := s.st.AllUsage(ctx)
	if err != nil {
		handleStoreErr(w, err)
		return
	}
	events, err := s.st.ListEvents(ctx, store.EventFilter{Limit: 20})
	if err != nil {
		handleStoreErr(w, err)
		return
	}

	lives := s.hub.AllLive()
	resp := overviewResponse{
		Nodes:    make([]*nodeView, 0, len(nodes)),
		Events:   events,
		ServerAt: time.Now().UTC(),
	}

	for _, n := range nodes {
		u := usages[n.ID]
		if u == nil {
			u = &store.Usage{NodeID: n.ID}
		}
		v := s.viewNode(n, u, lives[n.ID])
		resp.Nodes = append(resp.Nodes, v)

		resp.Summary.Total++
		switch n.Status {
		case store.StatusOnline:
			resp.Summary.Online++
		case store.StatusExceeded, store.StatusStopped:
			resp.Summary.Exceeded++
		case store.StatusOffline:
			resp.Summary.Offline++
		}
		if v.QuotaStatus.Warning {
			resp.Summary.Warning++
		}

		resp.Summary.TotalRx += u.RxBytes
		resp.Summary.TotalTx += u.TxBytes
		resp.Summary.TotalQuota += n.QuotaBytes
		if l := lives[n.ID]; l != nil && l.Connected {
			resp.Summary.TotalRate += int64(l.RxRate + l.TxRate)
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
