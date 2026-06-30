package dashboard

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/elkin/bestproxy/internal/proxy"
	"github.com/elkin/bestproxy/internal/stats"
)

//go:embed templates/dashboard.html
var templateFS embed.FS

type Handler struct {
	pools    []*proxy.Pool
	tmpl     *template.Template
}

func New(pools []*proxy.Pool) (*Handler, error) {
	tmpl, err := template.ParseFS(templateFS, "templates/dashboard.html")
	if err != nil {
		return nil, err
	}
	return &Handler{pools: pools, tmpl: tmpl}, nil
}

func (h *Handler) ServeJSON(w http.ResponseWriter, r *http.Request) {
	snap := h.buildJSONSnapshot()
	data, err := json.Marshal(snap)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (h *Handler) ServeDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	snap := h.buildTemplateData()
	if err := h.tmpl.Execute(w, snap); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (h *Handler) ServeEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			snap := h.buildJSONSnapshot()
			data, err := json.Marshal(snap)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// --- template data types ---

type templateData struct {
	Sets []templateSet
}

type templateSet struct {
	Name    string
	Proxies []templateProxy
}

type templateProxy struct {
	Addr          string
	SafeID        string
	Backup        bool
	TotalRequests int64
	ErrorCount    int64
	Avg1mStr      string
	Avg5mStr      string
	Avg1hStr      string
	Avg1mClass    string
	Avg5mClass    string
	Avg1hClass    string
	StatusClass   string
	StatusText    string
	// connection pool
	PoolInFlight  int64
	PoolIdle      int64
	PoolSize      int64
	PoolCreated   int64
}

func (h *Handler) buildTemplateData() templateData {
	var d templateData
	for _, p := range h.pools {
		ts := templateSet{Name: p.Name}
		for _, u := range p.Upstreams {
			snap := u.Stats.Snapshot()
			ts.Proxies = append(ts.Proxies, makeTemplateProxy(u.Addr, u.Backup, snap))
		}
		d.Sets = append(d.Sets, ts)
	}
	return d
}

func makeTemplateProxy(addr string, backup bool, snap stats.Snapshot) templateProxy {
	statusClass := "dot-up"
	statusText := "up"
	if !snap.LastCheckOK && snap.LastCheckAt != 0 {
		statusClass = "dot-down"
		statusText = "down"
	}

	return templateProxy{
		Addr:          addr,
		SafeID:        safeID(addr),
		Backup:        backup,
		TotalRequests: snap.TotalRequests,
		ErrorCount:    snap.ErrorCount,
		Avg1mStr:      fmtLatency(snap.Avg1m),
		Avg5mStr:      fmtLatency(snap.Avg5m),
		Avg1hStr:      fmtLatency(snap.Avg1h),
		Avg1mClass:    latencyClass(snap.Avg1m),
		Avg5mClass:    latencyClass(snap.Avg5m),
		Avg1hClass:    latencyClass(snap.Avg1h),
		StatusClass:   statusClass,
		StatusText:    statusText,
		PoolInFlight:  snap.Pool.InFlight,
		PoolIdle:      snap.Pool.Idle,
		PoolSize:      snap.Pool.PoolSize,
		PoolCreated:   snap.Pool.TotalCreated,
	}
}

// --- SSE JSON types ---

type jsonSnapshot struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Sets        []jsonSet    `json:"sets"`
}

type jsonSet struct {
	Name    string      `json:"name"`
	Proxies []jsonProxy `json:"proxies"`
}

type jsonProxy struct {
	Addr          string `json:"addr"`
	SafeID        string `json:"safe_id"`
	Backup        bool   `json:"backup"`
	TotalRequests int64  `json:"total_requests"`
	ErrorCount    int64  `json:"error_count"`
	Avg1mStr      string `json:"avg_1m_str"`
	Avg5mStr      string `json:"avg_5m_str"`
	Avg1hStr      string `json:"avg_1h_str"`
	Avg1mClass    string `json:"avg_1m_class"`
	Avg5mClass    string `json:"avg_5m_class"`
	Avg1hClass    string `json:"avg_1h_class"`
	StatusClass   string `json:"status_class"`
	StatusText    string `json:"status_text"`
	PoolInFlight  int64  `json:"pool_in_flight"`
	PoolIdle      int64  `json:"pool_idle"`
	PoolSize      int64  `json:"pool_size"`
	PoolCreated   int64  `json:"pool_created"`
}

func (h *Handler) buildJSONSnapshot() jsonSnapshot {
	snap := jsonSnapshot{GeneratedAt: time.Now()}
	for _, p := range h.pools {
		js := jsonSet{Name: p.Name}
		for _, u := range p.Upstreams {
			s := u.Stats.Snapshot()
			tp := makeTemplateProxy(u.Addr, u.Backup, s)
			js.Proxies = append(js.Proxies, jsonProxy{
				Addr:          tp.Addr,
				SafeID:        tp.SafeID,
				Backup:        tp.Backup,
				TotalRequests: tp.TotalRequests,
				ErrorCount:    tp.ErrorCount,
				Avg1mStr:      tp.Avg1mStr,
				Avg5mStr:      tp.Avg5mStr,
				Avg1hStr:      tp.Avg1hStr,
				Avg1mClass:    tp.Avg1mClass,
				Avg5mClass:    tp.Avg5mClass,
				Avg1hClass:    tp.Avg1hClass,
				StatusClass:   tp.StatusClass,
				StatusText:    tp.StatusText,
				PoolInFlight:  tp.PoolInFlight,
				PoolIdle:      tp.PoolIdle,
				PoolSize:      tp.PoolSize,
				PoolCreated:   tp.PoolCreated,
			})
		}
		snap.Sets = append(snap.Sets, js)
	}
	return snap
}

func fmtLatency(d time.Duration) string {
	if d == 0 {
		return "—"
	}
	if d < time.Millisecond {
		return fmt.Sprintf("%dµs", d.Microseconds())
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}

func latencyClass(d time.Duration) string {
	if d == 0 {
		return "none"
	}
	if d < 100*time.Millisecond {
		return "fast"
	}
	if d < 300*time.Millisecond {
		return "med"
	}
	return "slow"
}

func safeID(addr string) string {
	r := strings.NewReplacer(".", "-", ":", "-")
	return r.Replace(addr)
}
