// Copyright 2015 Prometheus Team
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/promlog"
	promlogflag "github.com/prometheus/common/promlog/flag"
	"github.com/prometheus/common/route"
	"github.com/prometheus/common/version"
	"github.com/prometheus/exporter-toolkit/web"
	webflag "github.com/prometheus/exporter-toolkit/web/kingpinflag"

	"github.com/prometheus/alertmanager/api"
	"github.com/prometheus/alertmanager/config"
	"github.com/prometheus/alertmanager/dispatch"
	"github.com/prometheus/alertmanager/inhibit"
	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/notify/email"
	"github.com/prometheus/alertmanager/notify/slack"
	"github.com/prometheus/alertmanager/notify/webhook"
	"github.com/prometheus/alertmanager/notify/wechat"
	"github.com/prometheus/alertmanager/provider/mem"
	"github.com/prometheus/alertmanager/silence"
	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/alertmanager/timeinterval"
	"github.com/prometheus/alertmanager/types"
	"github.com/prometheus/alertmanager/ui"
)

var (
	requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "alertmanager_http_request_duration_seconds",
			Help:    "Histogram of latencies for HTTP requests.",
			Buckets: []float64{.05, 0.1, .25, .5, .75, 1, 2, 5, 20, 60},
		},
		[]string{"handler", "method"},
	)
	responseSize = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "alertmanager_http_response_size_bytes",
			Help:    "Histogram of response size for HTTP requests.",
			Buckets: prometheus.ExponentialBuckets(100, 10, 7),
		},
		[]string{"handler", "method"},
	)
	clusterEnabled = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "alertmanager_cluster_enabled",
			Help: "Indicates whether the clustering is enabled or not.",
		},
	)
	configuredReceivers = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "alertmanager_receivers",
			Help: "Number of configured receivers.",
		},
	)
	configuredIntegrations = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "alertmanager_integrations",
			Help: "Number of configured integrations.",
		},
	)
	promlogConfig = promlog.Config{}
)

func init() {
	prometheus.MustRegister(requestDuration)
	prometheus.MustRegister(responseSize)
	prometheus.MustRegister(clusterEnabled)
	prometheus.MustRegister(configuredReceivers)
	prometheus.MustRegister(configuredIntegrations)
}

func instrumentHandler(handlerName string, handler http.HandlerFunc) http.HandlerFunc {
	handlerLabel := prometheus.Labels{"handler": handlerName}
	return promhttp.InstrumentHandlerDuration(
		requestDuration.MustCurryWith(handlerLabel),
		promhttp.InstrumentHandlerResponseSize(
			responseSize.MustCurryWith(handlerLabel),
			handler,
		),
	)
}

const defaultClusterAddr = "0.0.0.0:9094"

// buildReceiverIntegrations builds a list of integration notifiers off of a
// receiver config.
func buildReceiverIntegrations(nc config.Receiver, tmpl *template.Template, logger log.Logger) ([]*notify.Integration, error) {
	var (
		errs         types.MultiError
		integrations []*notify.Integration
		add          = func(name string, i int, rs notify.ResolvedSender, f func(l log.Logger) (notify.Notifier, error)) {
			n, err := f(log.With(logger, "integration", name))
			if err != nil {
				errs.Add(err)
				return
			}
			integrations = append(integrations, notify.NewIntegration(n, rs, name, i))
		}
	)

	for i, c := range nc.WebhookConfigs {
		add("webhook", i, c, func(l log.Logger) (notify.Notifier, error) { return webhook.New(c, tmpl, l) })
	}
	for i, c := range nc.EmailConfigs {
		add("email", i, c, func(l log.Logger) (notify.Notifier, error) { return email.New(c, tmpl, l), nil })
	}
	for i, c := range nc.WechatConfigs {
		add("wechat", i, c, func(l log.Logger) (notify.Notifier, error) { return wechat.New(c, tmpl, l) })
	}
	for i, c := range nc.SlackConfigs {
		add("slack", i, c, func(l log.Logger) (notify.Notifier, error) { return slack.New(c, tmpl, l) })
	}
	if errs.Len() > 0 {
		return nil, &errs
	}
	return integrations, nil
}

func main() {
	os.Exit(run())
}

func run() int {
	if os.Getenv("DEBUG") != "" {
		runtime.SetBlockProfileRate(20)
		runtime.SetMutexProfileFraction(20)
	}

	var (
		configFile      = kingpin.Flag("config.file", "Alertmanager configuration file name.").Default("alertmanager.yml").String()
		dataDir         = kingpin.Flag("storage.path", "Base path for data storage.").Default("data/").String()
		retention       = kingpin.Flag("data.retention", "How long to keep data for.").Default("120h").Duration()
		alertGCInterval = kingpin.Flag("alerts.gc-interval", "Interval between alert GC.").Default("30m").Duration()

		webConfig      = webflag.AddFlags(kingpin.CommandLine, ":9093")
		externalURL    = kingpin.Flag("web.external-url", "The URL under which Alertmanager is externally reachable (for example, if Alertmanager is served via a reverse proxy). Used for generating relative and absolute links back to Alertmanager itself. If the URL has a path portion, it will be used to prefix all HTTP endpoints served by Alertmanager. If omitted, relevant URL components will be derived automatically.").String()
		routePrefix    = kingpin.Flag("web.route-prefix", "Prefix for the internal routes of web endpoints. Defaults to path of --web.external-url.").String()
		getConcurrency = kingpin.Flag("web.get-concurrency", "Maximum number of GET requests processed concurrently. If negative or zero, the limit is GOMAXPROC or 8, whichever is larger.").Default("0").Int()
		httpTimeout    = kingpin.Flag("web.timeout", "Timeout for HTTP requests. If negative or zero, no timeout is set.").Default("0").Duration()
	)

	promlogflag.AddFlags(kingpin.CommandLine, &promlogConfig)
	kingpin.CommandLine.UsageWriter(os.Stdout)

	kingpin.Version(version.Print("alertmanager"))
	kingpin.CommandLine.GetFlag("help").Short('h')
	kingpin.Parse()

	logger := promlog.New(&promlogConfig)

	level.Info(logger).Log("msg", "Starting Alertmanager", "version", version.Info())
	level.Info(logger).Log("build_context", version.BuildContext())

	err := os.MkdirAll(*dataDir, 0o777)
	if err != nil {
		level.Error(logger).Log("msg", "Unable to create data directory", "err", err)
		return 1
	}

	stopc := make(chan struct{})
	var wg sync.WaitGroup

	marker := types.NewMarker(prometheus.DefaultRegisterer)

	silenceOpts := silence.Options{
		Logger:  log.With(logger, "component", "silences"),
		Metrics: prometheus.DefaultRegisterer,
	}

	silences, err := silence.New(silenceOpts)
	if err != nil {
		level.Error(logger).Log("err", err)
		return 1
	}

	defer func() {
		close(stopc)
		wg.Wait()
	}()

	alerts, err := mem.NewAlerts(context.Background(), marker, *alertGCInterval, nil, logger, prometheus.DefaultRegisterer)
	if err != nil {
		level.Error(logger).Log("err", err)
		return 1
	}
	defer alerts.Close()

	var disp *dispatch.Dispatcher
	defer func() {
		disp.Stop()
	}()

	groupFn := func(routeFilter func(*dispatch.Route) bool, alertFilter func(*types.Alert, time.Time) bool) (dispatch.AlertGroups, map[model.Fingerprint][]string) {
		return disp.Groups(routeFilter, alertFilter)
	}

	api, err := api.New(api.Options{
		Alerts:      alerts,
		Silences:    silences,
		StatusFunc:  marker.Status,
		Timeout:     *httpTimeout,
		Concurrency: *getConcurrency,
		Logger:      log.With(logger, "component", "api"),
		Registry:    prometheus.DefaultRegisterer,
		GroupFunc:   groupFn,
	})
	if err != nil {
		level.Error(logger).Log("err", errors.Wrap(err, "failed to create API"))
		return 1
	}

	amURL, err := extURL(logger, os.Hostname, (*webConfig.WebListenAddresses)[0], *externalURL)
	if err != nil {
		level.Error(logger).Log("msg", "failed to determine external URL", "err", err)
		return 1
	}
	level.Debug(logger).Log("externalURL", amURL.String())

	waitFunc := func() time.Duration { return 0 }

	timeoutFunc := func(d time.Duration) time.Duration {
		if d < notify.MinTimeout {
			d = notify.MinTimeout
		}
		return d + waitFunc()
	}

	var (
		inhibitor *inhibit.Inhibitor
		tmpl      *template.Template
	)

	dispMetrics := dispatch.NewDispatcherMetrics(false, prometheus.DefaultRegisterer)
	pipelineBuilder := notify.NewPipelineBuilder(prometheus.DefaultRegisterer)
	configLogger := log.With(logger, "component", "configuration")
	configCoordinator := config.NewCoordinator(
		*configFile,
		prometheus.DefaultRegisterer,
		configLogger,
	)
	configCoordinator.Subscribe(func(conf *config.Config) error {
		tmpl, err = template.FromGlobs(conf.Templates)
		if err != nil {
			return errors.Wrap(err, "failed to parse templates")
		}
		tmpl.ExternalURL = amURL

		// Build the routing tree and record which receivers are used.
		routes := dispatch.NewRoute(conf.Route, nil)
		activeReceiversMap := make(map[string]struct{})
		routes.Walk(func(r *dispatch.Route) {
			activeReceiversMap[r.RouteOpts.Receiver] = struct{}{}
		})

		// Build the map of receiver to integrations.
		receivers := make([]*notify.Receiver, 0, len(activeReceiversMap))
		var integrationsNum int
		for _, rcv := range conf.Receivers {
			if _, found := activeReceiversMap[rcv.Name]; !found {
				// No need to build a receiver if no route is using it.
				level.Info(configLogger).Log("msg", "skipping creation of receiver not referenced by any route", "receiver", rcv.Name)
				receivers = append(receivers, notify.NewReceiver(rcv.Name, false, nil))
				continue
			}
			integrations, err := buildReceiverIntegrations(rcv, tmpl, logger)
			if err != nil {
				return err
			}
			receivers = append(receivers, notify.NewReceiver(rcv.Name, true, integrations))
			integrationsNum += len(integrations)
		}

		// Build the map of time interval names to time interval definitions.
		timeIntervals := make(map[string][]timeinterval.TimeInterval, len(conf.MuteTimeIntervals)+len(conf.TimeIntervals))
		for _, ti := range conf.MuteTimeIntervals {
			timeIntervals[ti.Name] = ti.TimeIntervals
		}

		for _, ti := range conf.TimeIntervals {
			timeIntervals[ti.Name] = ti.TimeIntervals
		}

		inhibitor.Stop()
		disp.Stop()

		inhibitor = inhibit.NewInhibitor(alerts, conf.InhibitRules, marker, logger)
		silencer := silence.NewSilencer(silences, marker, logger)

		activeReceivers := make([]*notify.Receiver, 0, len(receivers))
		for i := range receivers {
			if !receivers[i].Active() {
				continue
			}
			activeReceivers = append(activeReceivers, receivers[i])
		}

		pipeline := pipelineBuilder.New(
			nil,
			activeReceivers,
			inhibitor,
			silencer,
			timeIntervals,
		)
		configuredReceivers.Set(float64(len(activeReceiversMap)))
		configuredIntegrations.Set(float64(integrationsNum))

		api.Update(conf, receivers, func(labels model.LabelSet) {
			inhibitor.Mutes(labels)
			silencer.Mutes(labels)
		})

		disp = dispatch.NewDispatcher(alerts, routes, pipeline, marker, timeoutFunc, nil, logger, dispMetrics)
		routes.Walk(func(r *dispatch.Route) {
			if r.RouteOpts.RepeatInterval > *retention {
				level.Warn(configLogger).Log(
					"msg",
					"repeat_interval is greater than the data retention period. It can lead to notifications being repeated more often than expected.",
					"repeat_interval",
					r.RouteOpts.RepeatInterval,
					"retention",
					*retention,
					"route",
					r.Key(),
				)
			}

			if r.RouteOpts.RepeatInterval < r.RouteOpts.GroupInterval {
				level.Warn(configLogger).Log(
					"msg",
					"repeat_interval is less than group_interval. Notifications will not repeat until the next group_interval.",
					"repeat_interval",
					r.RouteOpts.RepeatInterval,
					"group_interval",
					r.RouteOpts.GroupInterval,
					"route",
					r.Key(),
				)
			}
		})

		go disp.Run()
		go inhibitor.Run()

		return nil
	})

	if err := configCoordinator.Reload(); err != nil {
		return 1
	}

	// Make routePrefix default to externalURL path if empty string.
	if *routePrefix == "" {
		*routePrefix = amURL.Path
	}
	*routePrefix = "/" + strings.Trim(*routePrefix, "/")
	level.Debug(logger).Log("routePrefix", *routePrefix)

	router := route.New().WithInstrumentation(instrumentHandler)
	if *routePrefix != "/" {
		router.Get("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, *routePrefix, http.StatusFound)
		})
		router = router.WithPrefix(*routePrefix)
	}

	webReload := make(chan chan error)

	ui.Register(router, webReload, logger)

	mux := api.Register(router, *routePrefix)

	srv := &http.Server{Handler: mux}
	srvc := make(chan struct{})

	go func() {
		if err := web.ListenAndServe(srv, webConfig, logger); err != http.ErrServerClosed {
			level.Error(logger).Log("msg", "Listen error", "err", err)
			close(srvc)
		}
		defer func() {
			if err := srv.Close(); err != nil {
				level.Error(logger).Log("msg", "Error on closing the server", "err", err)
			}
		}()
	}()

	var (
		hup  = make(chan os.Signal, 1)
		term = make(chan os.Signal, 1)
	)
	signal.Notify(hup, syscall.SIGHUP)
	signal.Notify(term, os.Interrupt, syscall.SIGTERM)

	for {
		select {
		case <-hup:
			// ignore error, already logged in `reload()`
			_ = configCoordinator.Reload()
		case errc := <-webReload:
			errc <- configCoordinator.Reload()
		case <-term:
			level.Info(logger).Log("msg", "Received SIGTERM, exiting gracefully...")
			return 0
		case <-srvc:
			return 1
		}
	}
}

func extURL(logger log.Logger, hostnamef func() (string, error), listen, external string) (*url.URL, error) {
	if external == "" {
		hostname, err := hostnamef()
		if err != nil {
			return nil, err
		}
		_, port, err := net.SplitHostPort(listen)
		if err != nil {
			return nil, err
		}
		if port == "" {
			level.Warn(logger).Log("msg", "no port found for listen address", "address", listen)
		}

		external = fmt.Sprintf("http://%s:%s/", hostname, port)
	}

	u, err := url.Parse(external)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.Errorf("%q: invalid %q scheme, only 'http' and 'https' are supported", u.String(), u.Scheme)
	}

	ppref := strings.TrimRight(u.Path, "/")
	if ppref != "" && !strings.HasPrefix(ppref, "/") {
		ppref = "/" + ppref
	}
	u.Path = ppref

	return u, nil
}
