// Package metrics exposes Prometheus metrics for the server and, optionally, the
// net/http/pprof profiling endpoints. Counters are read from the media plane's atomic
// statistics at scrape time, so the packet path only touches two lock-free histograms.
package metrics

import (
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/media"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/room"
	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/signaling"
)

const namespace = "confserver"

// Metrics owns the Prometheus registry of one server.
type Metrics struct {
	Registry       *prometheus.Registry
	forwardLatency prometheus.Histogram
	mixTick        prometheus.Histogram
}

// New creates the registry with the Go runtime and process collectors and the
// latency histograms.
func New() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		forwardLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "forward_latency_seconds",
			Help:      "Time from receiving a video packet to the last forwarded copy being written.",
			Buckets:   []float64{2e-6, 5e-6, 1e-5, 2.5e-5, 5e-5, 1e-4, 2.5e-4, 5e-4, 1e-3, 2.5e-3, 1e-2},
		}),
		mixTick: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "mixer_tick_seconds",
			Help:      "Duration of one 20 ms audio mixer tick for one room.",
			Buckets:   []float64{1e-6, 5e-6, 1e-5, 2.5e-5, 5e-5, 1e-4, 2.5e-4, 5e-4, 1e-3, 5e-3},
		}),
	}
	m.Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.forwardLatency,
		m.mixTick,
	)
	return m
}

// Hooks returns the media hooks that feed the latency histograms.
func (m *Metrics) Hooks() media.Hooks {
	return media.Hooks{
		ForwardLatency: func(d time.Duration) { m.forwardLatency.Observe(d.Seconds()) },
		MixTick:        func(d time.Duration) { m.mixTick.Observe(d.Seconds()) },
	}
}

// Attach registers the collector for the router, registry and control server.
func (m *Metrics) Attach(router *media.Router, reg *room.Registry, sig *signaling.Server) {
	m.Registry.MustRegister(newCollector(router, reg, sig))
}

// Handler serves /metrics and, when enablePprof is set, /debug/pprof/.
func (m *Metrics) Handler(enablePprof bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	if enablePprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	return mux
}

type collector struct {
	router *media.Router
	reg    *room.Registry
	sig    *signaling.Server

	packetsIn, bytesIn, packetsOut, bytesOut, drops    *prometheus.Desc
	mixerTicks, mixerOverruns, fecRecovered, concealed *prometheus.Desc
	rooms, participants, controlConns, slowDisconnects *prometheus.Desc
	controlConnsTotal                                  *prometheus.Desc
}

func newCollector(router *media.Router, reg *room.Registry, sig *signaling.Server) *collector {
	d := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(prometheus.BuildFQName(namespace, "", name), help, labels, nil)
	}
	return &collector{
		router:            router,
		reg:               reg,
		sig:               sig,
		packetsIn:         d("media_packets_received_total", "UDP datagrams accepted by kind.", "kind"),
		bytesIn:           d("media_bytes_received_total", "UDP payload bytes accepted by kind.", "kind"),
		packetsOut:        d("media_packets_sent_total", "UDP datagrams sent by kind.", "kind"),
		bytesOut:          d("media_bytes_sent_total", "UDP payload bytes sent by kind.", "kind"),
		drops:             d("media_dropped_packets_total", "UDP datagrams dropped by reason.", "reason"),
		mixerTicks:        d("mixer_ticks_total", "Mixer ticks that produced audio."),
		mixerOverruns:     d("mixer_overrun_ticks_total", "Mixer ticks skipped because the mixer fell behind."),
		fecRecovered:      d("audio_fec_recovered_total", "Upstream audio packets rebuilt from FEC parity."),
		concealed:         d("audio_concealed_frames_total", "Sender frames concealed by the mixer's jitter buffer."),
		rooms:             d("rooms", "Open rooms by media mode.", "mode"),
		participants:      d("participants", "Members of open rooms."),
		controlConns:      d("control_connections", "Open control connections."),
		controlConnsTotal: d("control_connections_total", "Control connections accepted."),
		slowDisconnects:   d("control_slow_disconnects_total", "Control connections closed because their outbox overflowed."),
	}
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		c.packetsIn, c.bytesIn, c.packetsOut, c.bytesOut, c.drops,
		c.mixerTicks, c.mixerOverruns, c.fecRecovered, c.concealed,
		c.rooms, c.participants, c.controlConns, c.controlConnsTotal, c.slowDisconnects,
	} {
		ch <- d
	}
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	st := c.router.Stats()
	counter := func(d *prometheus.Desc, v uint64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, float64(v), labels...)
	}
	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	for _, k := range media.InKinds() {
		counter(c.packetsIn, st.PacketsIn[k].Load(), k.String())
		counter(c.bytesIn, st.BytesIn[k].Load(), k.String())
	}
	for _, k := range media.OutKinds() {
		counter(c.packetsOut, st.PacketsOut[k].Load(), k.String())
		counter(c.bytesOut, st.BytesOut[k].Load(), k.String())
	}
	for _, r := range media.DropReasons() {
		counter(c.drops, st.Drops[r].Load(), r.String())
	}
	counter(c.mixerTicks, st.MixerTicks.Load())
	counter(c.mixerOverruns, st.MixerOverruns.Load())
	counter(c.fecRecovered, st.FECRecovered.Load())
	counter(c.concealed, st.AudioConcealed.Load())

	view := c.reg.View()
	byMode := map[string]int{room.ModeRelay.String(): 0, room.ModeP2P.String(): 0}
	members := 0
	for i := range view.Rooms {
		byMode[view.Rooms[i].Mode.String()]++
		members += len(view.Rooms[i].Members)
	}
	for mode, n := range byMode {
		gauge(c.rooms, float64(n), mode)
	}
	gauge(c.participants, float64(members))
	gauge(c.controlConns, float64(c.sig.ActiveConns()))
	counter(c.controlConnsTotal, c.sig.TotalConns())
	counter(c.slowDisconnects, c.sig.SlowDisconnects())
}
