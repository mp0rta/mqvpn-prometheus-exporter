// Command mqvpn-prometheus-exporter is a sidecar Prometheus exporter for the
// mqvpn multipath QUIC VPN server. It polls mqvpn's JSON control API and
// exposes /metrics in Prometheus exposition format. See docs/README.md for
// the operator guide and exposed metric reference.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mp0rta/mqvpn-prometheus-exporter/client"
	"github.com/mp0rta/mqvpn-prometheus-exporter/collector"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// exporterVersion is overridden at build time via -ldflags "-X main.exporterVersion=..."
// (see .goreleaser.yaml). Default is "0.5.0-dev" so that `go install`-style
// builds without the ldflags override surface as a dev build in
// mqvpn_exporter_build_info{version=...}, distinguishable from a tagged GA.
var exporterVersion = "0.5.0-dev"

func main() {
	var (
		listenAddr = flag.String("web.listen-address", "127.0.0.1:9091",
			"Address on which to expose /metrics. Use 0.0.0.0:9091 to expose externally (front with nginx for auth).")
		mqvpnAddr = flag.String("mqvpn.address", "127.0.0.1:9090",
			"mqvpn control API address (host:port).")
		timeout = flag.Duration("mqvpn.timeout", 5*time.Second,
			"Per-call timeout when talking to mqvpn.")
		scrapeBudget = flag.Duration("mqvpn.scrape-budget", collector.DefaultScrapeBudget,
			"Overall budget for one Prometheus scrape (sum of all RPCs). Must stay below your Prometheus scrape_interval.")
		includeEndpoint = flag.Bool("metrics.include-endpoint", false,
			"Emit mqvpn_client_info{user, endpoint}=1. Off by default — mobile/NAT clients can rebind endpoints frequently and inflate Prometheus series cardinality.")
	)
	flag.Parse()

	if !strings.HasPrefix(*listenAddr, "127.0.0.1") &&
		!strings.HasPrefix(*listenAddr, "localhost") {
		log.Printf("WARN: --web.listen-address %q is non-loopback — this exporter has no auth, front it with nginx", *listenAddr)
	}

	cli := client.New(*mqvpnAddr, *timeout)
	coll := collector.New(collector.Config{
		Source:          cli,
		Budget:          *scrapeBudget,
		IncludeEndpoint: *includeEndpoint,
	})

	reg := prometheus.NewRegistry()
	reg.MustRegister(coll)
	// Modern accessors (client_golang >= v1.12). The older
	// prometheus.NewGoCollector / NewProcessCollector are deprecated.
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	// Promised by spec: mqvpn_exporter_build_info{version=...} = 1.
	// Distinct from go_build_info (which collectors.NewGoCollector emits).
	exporterBuild := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mqvpn_exporter_build_info",
		Help: "mqvpn-prometheus-exporter build info; value is always 1.",
	}, []string{"version"})
	exporterBuild.WithLabelValues(exporterVersion).Set(1)
	reg.MustRegister(exporterBuild)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, "mqvpn-prometheus-exporter %s\n\nSee /metrics, /healthz\n", exporterVersion)
	})

	log.Printf("listening on %s, scraping mqvpn at %s (timeout=%s)",
		*listenAddr, *mqvpnAddr, *timeout)
	srv := &http.Server{
		Addr:              *listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
