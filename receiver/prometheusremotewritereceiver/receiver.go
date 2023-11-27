// Copyright  The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prometheusremotewritereceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/prometheusremotewritereceiver"

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/receiverhelper"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/translator/prometheusremotewrite"
)

const (
	receiverFormat = "protobuf"
)

//var reg = regexp.MustCompile(`(\w+)_(\w+)_(\w+)\z`)

// PrometheusRemoteWriteReceiver - remote write
type PrometheusRemoteWriteReceiver struct {
	params       receiver.CreateSettings
	host         component.Host
	nextConsumer consumer.Metrics

	mu         sync.Mutex
	startOnce  sync.Once
	stopOnce   sync.Once
	shutdownWG sync.WaitGroup

	server        *http.Server
	config        *Config
	timeThreshold *int64
	logger        *zap.Logger
	obsrecv       *receiverhelper.ObsReport
}

// NewReceiver - remote write
func NewReceiver(params receiver.CreateSettings, config *Config, consumer consumer.Metrics) (*PrometheusRemoteWriteReceiver, error) {
	obsrecv, err := receiverhelper.NewObsReport(receiverhelper.ObsReportSettings{
		ReceiverID:             params.ID,
		ReceiverCreateSettings: params,
	})
	zr := &PrometheusRemoteWriteReceiver{
		params:        params,
		nextConsumer:  consumer,
		config:        config,
		logger:        params.Logger,
		obsrecv:       obsrecv,
		timeThreshold: &config.TimeThreshold,
	}
	return zr, err
}

func noopDecoder(body io.ReadCloser) (io.ReadCloser, error) {
	return body, nil
}

// Start - remote write
func (rec *PrometheusRemoteWriteReceiver) Start(_ context.Context, host component.Host) error {
	if host == nil {
		return errors.New("nil host")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var err = component.ErrNilNextConsumer
	rec.startOnce.Do(func() {
		err = nil
		rec.host = host
		rec.server, err = rec.config.HTTPServerSettings.ToServer(host, rec.params.TelemetrySettings, rec, confighttp.WithDecoder("snappy", noopDecoder))
		var listener net.Listener
		listener, err = rec.config.HTTPServerSettings.ToListener()
		if err != nil {
			return
		}
		rec.shutdownWG.Add(1)
		go func() {
			defer rec.shutdownWG.Done()
			if errHTTP := rec.server.Serve(listener); errHTTP != http.ErrServerClosed {
				host.ReportFatalError(errHTTP)
			}
		}()
	})
	return err
}

type pooledDecoder struct {
	bufferPool       sync.Pool
	writeRequestPool sync.Pool
}

func (pd *pooledDecoder) acquireAndDecode(r io.Reader) (*prompb.WriteRequest, error) {
	rawBuffer := pd.bufferPool.Get().(*bytes.Buffer)
	rawBuffer.Reset()
	defer pd.bufferPool.Put(rawBuffer)

	if _, err := rawBuffer.ReadFrom(r); err != nil {
		return nil, err
	}

	decBuffer := pd.bufferPool.Get().(*bytes.Buffer)
	decBuffer.Reset()
	defer pd.bufferPool.Put(decBuffer)

	decoded, err := snappy.Decode(decBuffer.AvailableBuffer(), rawBuffer.Bytes())
	if err != nil {
		return nil, err
	}

	req := pd.writeRequestPool.Get().(*prompb.WriteRequest)
	if err := proto.Unmarshal(decoded, req); err != nil {
		return nil, err
	}

	return req, nil
}

func (pd *pooledDecoder) release(req *prompb.WriteRequest) {
	req.Reset()
	pd.writeRequestPool.Put(req)
}

var pd = pooledDecoder{
	bufferPool: sync.Pool{New: func() any {
		return &bytes.Buffer{}
	}},
	writeRequestPool: sync.Pool{New: func() any {
		return &prompb.WriteRequest{}
	}},
}

func (rec *PrometheusRemoteWriteReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := rec.obsrecv.StartMetricsOp(r.Context())

	req, err := pd.acquireAndDecode(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()
	defer pd.release(req)

	pms, err := prometheusremotewrite.FromTimeSeries(req.Timeseries, prometheusremotewrite.FromPRWSettings{
		TimeThreshold: *rec.timeThreshold,
		Logger:        *rec.logger,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	metricCount := pms.ResourceMetrics().Len()
	dataPointCount := pms.DataPointCount()
	if metricCount != 0 {
		err = rec.nextConsumer.ConsumeMetrics(ctx, pms)
	}
	rec.obsrecv.EndMetricsOp(ctx, receiverFormat, dataPointCount, err)
	w.WriteHeader(http.StatusAccepted)
}

// Shutdown - remote write
func (rec *PrometheusRemoteWriteReceiver) Shutdown(context.Context) error {
	var err = component.ErrNilNextConsumer
	rec.stopOnce.Do(func() {
		err = rec.server.Close()
		rec.shutdownWG.Wait()
	})
	return err
}
