// Copyright 2017 The Prometheus Authors
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

//
// Copyright 2019 Crunchy Data
//
// Based on the Prometheus remote storage example:
// documentation/examples/remote_storage/remote_storage_adapter/main.go
//

// The main package for the Prometheus server executable.
package main


import (
	"flag"
	"io/ioutil"
	"net/http"
	_ "net/http/pprof"
	"os"
  "os/signal"
  "sync/atomic"
	"time"

	"github.com/crunchydata/postgresql-prometheus-adapter/pkg/log"
	"github.com/crunchydata/postgresql-prometheus-adapter/pkg/postgresql"
	"github.com/crunchydata/postgresql-prometheus-adapter/pkg/util"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/jamiealquiza/envy"

	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_model/go"
	"github.com/prometheus/common/model"

	"fmt"
)

type config struct {
	remoteTimeout      time.Duration
	listenAddr         string
	telemetryPath      string
	pgPrometheusConfig postgresql.Config
	logLevel           string
	haGroupLockId      int
	prometheusTimeout  time.Duration
}

const (
	tickInterval      = time.Second
	promLivenessCheck = time.Second
)

var (
	receivedSamples = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "received_samples_total",
			Help: "Total number of received samples.",
		},
	)
	sentSamples = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sent_samples_total",
			Help: "Total number of processed samples sent to remote storage.",
		},
		[]string{"remote"},
	)
	failedSamples = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "failed_samples_total",
			Help: "Total number of processed samples which failed on send to remote storage.",
		},
		[]string{"remote"},
	)
	sentBatchDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sent_batch_duration_seconds",
			Help:    "Duration of sample batch send calls to the remote storage.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"remote"},
	)
	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_ms",
			Help:    "Duration of HTTP request in milliseconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"path"},
	)
	writeThroughput     = util.NewThroughputCalc(tickInterval)
	lastRequestUnixNano = time.Now().UnixNano()
)

var worker *postgresql.Client

func init() {
	prometheus.MustRegister(receivedSamples)
	prometheus.MustRegister(sentSamples)
	prometheus.MustRegister(failedSamples)
	prometheus.MustRegister(sentBatchDuration)
	prometheus.MustRegister(httpRequestDuration)
	writeThroughput.Start()
}

func main() {
	cfg := parseFlags()
	log.Init(cfg.logLevel)
	log.Info("config", fmt.Sprintf("%+v", cfg))

	http.Handle(cfg.telemetryPath, promhttp.Handler())
	writer, reader := buildClients(cfg)

  c := make(chan os.Signal, 1)
  signal.Notify(c, os.Interrupt)
  go func(){
      for sig := range c {
        fmt.Printf("Signal: %v\n", sig)
        worker.Shutdown()
      }
  }()

  go worker.Run()
  defer worker.Shutdown()

  log.Info("msg", "Starting HTTP Listerner")

	http.Handle("/write", timeHandler("write", write(writer)))
	http.Handle("/read", timeHandler("read", read(reader)))
	http.Handle("/healthz", health(reader))

	log.Info("msg", "Starting up...")
	log.Info("msg", "Listening", "addr", cfg.listenAddr)

  err := http.ListenAndServe(cfg.listenAddr, nil)


	if err != nil {
		log.Error("msg", "Listen failure", "err", err)
		os.Exit(1)
	}
}

func parseFlags() *config {

	cfg := &config{}

	postgresql.ParseFlags(&cfg.pgPrometheusConfig)

	flag.DurationVar(&cfg.remoteTimeout, "adapter-send-timeout", 30*time.Second, "The timeout to use when sending samples to the remote storage.")
	flag.StringVar(&cfg.listenAddr, "web-listen-address", ":9201", "Address to listen on for web endpoints.")
	flag.StringVar(&cfg.telemetryPath, "web-telemetry-path", "/metrics", "Address to listen on for web endpoints.")
	flag.StringVar(&cfg.logLevel, "log-level", "debug", "The log level to use [ \"error\", \"warn\", \"info\", \"debug\" ].")

	envy.Parse("TS_PROM")
	flag.Parse()

	return cfg
}

type writer interface {
	Write(samples model.Samples) error
	Name() string
}

type reader interface {
	Read(req *prompb.ReadRequest) (*prompb.ReadResponse, error)
	Name() string
	HealthCheck() error
}

func buildClients(cfg *config) (writer, reader) {
	pgClient := postgresql.NewClient(&cfg.pgPrometheusConfig)

  worker = pgClient

	return pgClient, pgClient
}

func write(writer writer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		compressed, err := ioutil.ReadAll(r.Body)
		if err != nil {
			log.Error("msg", "Read error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		reqBuf, err := snappy.Decode(nil, compressed)
		if err != nil {
			log.Error("msg", "Decode error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var req prompb.WriteRequest
		if err := proto.Unmarshal(reqBuf, &req); err != nil {
			log.Error("msg", "Unmarshal error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		samples := protoToSamples(&req)
		receivedSamples.Add(float64(len(samples)))

		err = sendSamples(writer, samples)
		if err != nil {
			log.Warn("msg", "Error sending samples to remote storage", "err", err, "storage", writer.Name(), "num_samples", len(samples))
		}

		counter, err := sentSamples.GetMetricWithLabelValues(writer.Name())
		if err != nil {
			log.Warn("msg", "Couldn't get a counter", "labelValue", writer.Name(), "err", err)
		}
		writeThroughput.SetCurrent(getCounterValue(counter))

		/*select {
		case d := <-writeThroughput.Values:
			log.Info("msg", "Samples write throughput", "samples/sec", d)
		default:
		}*/
	})
}

func getCounterValue(counter prometheus.Counter) float64 {
	dtoMetric := &io_prometheus_client.Metric{}
	if err := counter.Write(dtoMetric); err != nil {
		log.Warn("msg", "Error reading counter value", "err", err, "sentSamples", sentSamples)
	}
	return dtoMetric.GetCounter().GetValue()
}

func read(reader reader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		compressed, err := ioutil.ReadAll(r.Body)
		if err != nil {
			log.Error("msg", "Read error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		reqBuf, err := snappy.Decode(nil, compressed)
		if err != nil {
			log.Error("msg", "Decode error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var req prompb.ReadRequest
		if err := proto.Unmarshal(reqBuf, &req); err != nil {
			log.Error("msg", "Unmarshal error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var resp *prompb.ReadResponse
		resp, err = reader.Read(&req)
		if err != nil {
      fmt.Printf("MAIN req.Queries: %v\n", req.Queries)
			log.Warn("msg", "Error executing query", "query", req, "storage", reader.Name(), "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		data, err := proto.Marshal(resp)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Header().Set("Content-Encoding", "snappy")

		compressed = snappy.Encode(nil, data)
		if _, err := w.Write(compressed); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	})
}

func health(reader reader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := reader.HealthCheck()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Length", "0")
	})
}

func protoToSamples(req *prompb.WriteRequest) model.Samples {
	var samples model.Samples
	for _, ts := range req.Timeseries {
		metric := make(model.Metric, len(ts.Labels))
		for _, l := range ts.Labels {
			metric[model.LabelName(l.Name)] = model.LabelValue(l.Value)
		}

		for _, s := range ts.Samples {
			samples = append(samples, &model.Sample{
				Metric:    metric,
				Value:     model.SampleValue(s.Value),
				Timestamp: model.Time(s.Timestamp),
			})
		}
	}
	return samples
}

func sendSamples(w writer, samples model.Samples) error {
	atomic.StoreInt64(&lastRequestUnixNano, time.Now().UnixNano())
	begin := time.Now()
	var err error
  err = w.Write(samples)
	duration := time.Since(begin).Seconds()
	if err != nil {
		failedSamples.WithLabelValues(w.Name()).Add(float64(len(samples)))
		return err
	}
	sentSamples.WithLabelValues(w.Name()).Add(float64(len(samples)))
	sentBatchDuration.WithLabelValues(w.Name()).Observe(duration)
	return nil
}

// timeHandler uses Prometheus histogram to track request time
func timeHandler(path string, handler http.Handler) http.Handler {
	f := func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		handler.ServeHTTP(w, r)
		elapsedMs := time.Since(start).Nanoseconds() / int64(time.Millisecond)
		httpRequestDuration.WithLabelValues(path).Observe(float64(elapsedMs))
	}
	return http.HandlerFunc(f)
}
