// SPDX-License-Identifier: BSD-3-Clause

// Package otlplogs exports the local log stream with the official OTLP SDK.
package otlplogs

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/buildinfo"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/logging"
	"github.com/imksoo/routerd2/internal/protocol/otlplogconfig"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const operationTimeout = time.Second

// Run uses a private provider and SDK-owned bounded queue. Neither collector
// availability nor export acknowledgements gate local logging or worker ready.
// Initial startup and a logger epoch change begin at the latest local cursor.
func Run(ctx context.Context, cfg otlplogconfig.Config, runtimeDir string, ready func()) error {
	cfg = otlplogconfig.Normalize(cfg)
	if err := otlplogconfig.Validate(cfg); err != nil {
		return err
	}
	endpoint := cfg.Endpoint
	plaintext := cfg.Insecure
	if strings.Contains(endpoint, "://") {
		u, _ := url.Parse(endpoint) // Validate established the endpoint syntax.
		endpoint = u.Host
		plaintext = plaintext || u.Scheme == "http"
	}
	var transport credentials.TransportCredentials = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	if plaintext {
		transport = insecure.NewCredentials()
	}
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(transport))
	if err != nil {
		return err
	}
	defer conn.Close()
	exporter, err := otlploggrpc.New(ctx,
		otlploggrpc.WithGRPCConn(conn), otlploggrpc.WithEndpoint(endpoint),
		otlploggrpc.WithHeaders(cfg.Headers), otlploggrpc.WithCompressor("none"),
		otlploggrpc.WithTimeout(operationTimeout), otlploggrpc.WithRetry(otlploggrpc.RetryConfig{Enabled: false}),
	)
	if err != nil {
		return err
	}
	attrs := []attribute.KeyValue{attribute.String("service.name", cfg.ServiceName), attribute.String("service.version", buildinfo.ReleaseID)}
	if cfg.ServiceNamespace != "" {
		attrs = append(attrs, attribute.String("service.namespace", cfg.ServiceNamespace))
	}
	if hostname, err := os.Hostname(); err == nil {
		attrs = append(attrs, attribute.String("host.name", hostname))
	}
	for key, value := range cfg.Attributes {
		attrs = append(attrs, attribute.String(key, value))
	}
	provider := sdklog.NewLoggerProvider(
		sdklog.WithResource(resource.NewSchemaless(attrs...)),
		sdklog.WithAttributeCountLimit(128), sdklog.WithAttributeValueLengthLimit(-1),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter,
			sdklog.WithMaxQueueSize(1024), sdklog.WithExportMaxBatchSize(logging.MaxQuery),
			sdklog.WithExportInterval(250*time.Millisecond), sdklog.WithExportTimeout(operationTimeout), sdklog.WithExportBufferSize(1))),
	)
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := provider.Shutdown(stop); err != nil {
			slog.Warn("OTLP log export shutdown failed", "error", err)
		}
	}()
	logger := provider.Logger("routerd2.local-log")
	client := localapi.NewClient(filepath.Join(runtimeDir, logging.SocketName))
	defer client.HTTP.CloseIdleConnections()
	var cursor logging.Cursor
	queryFailed := false
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		page, err := query(ctx, client, cfg.MinLevel, cursor)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !queryFailed {
				slog.Warn("OTLP local log query failed", "error", err)
				queryFailed = true
			}
			timer.Reset(time.Second)
			continue
		}
		if queryFailed {
			slog.Info("OTLP local log queries resumed")
			queryFailed = false
		}
		if cursor.Epoch == "" || page.Reset {
			first := cursor.Epoch == ""
			cursor = logging.Cursor{Epoch: page.Cursor.Epoch, Sequence: page.LatestSequence}
			if first && ready != nil {
				ready()
			}
		} else {
			for _, entry := range page.Records {
				// SDK diagnostics must not feed their own failing exporter.
				if entry.PID == os.Getpid() {
					continue
				}
				logger.Emit(ctx, record(entry))
			}
			cursor = page.Cursor
		}
		delay := 250 * time.Millisecond
		if page.More {
			delay = 0
		}
		timer.Reset(delay)
	}
}

func query(ctx context.Context, client *localapi.Client, level string, cursor logging.Cursor) (logging.QueryResult, error) {
	level = strings.ToUpper(level)
	if level == "WARNING" {
		level = "WARN"
	}
	values := url.Values{"limit": {strconv.Itoa(logging.MaxQuery)}, "level": {level}}
	if cursor.Epoch == "" {
		values.Set("tail", "true")
		values.Set("limit", "1")
	} else {
		values.Set("epoch", cursor.Epoch)
		values.Set("after", strconv.FormatUint(cursor.Sequence, 10))
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	var page logging.QueryResult
	err := client.Do(ctx, http.MethodGet, "/v1/logs?"+values.Encode(), nil, &page)
	return page, err
}

func record(entry logging.Record) log.Record {
	var r log.Record
	r.SetTimestamp(entry.Timestamp)
	r.SetObservedTimestamp(time.Now())
	r.SetBody(log.StringValue(entry.Message))
	r.SetSeverityText(entry.Level)
	switch entry.Level {
	case "DEBUG":
		r.SetSeverity(log.SeverityDebug)
	case "WARN":
		r.SetSeverity(log.SeverityWarn)
	case "ERROR":
		r.SetSeverity(log.SeverityError)
	default:
		r.SetSeverity(log.SeverityInfo)
	}
	r.AddAttributes(
		log.String("routerd.module", entry.Module), log.Int("process.pid", entry.PID),
		log.String("routerd.generation", strconv.FormatUint(entry.Generation, 10)),
		log.String("routerd.log.epoch", entry.Epoch), log.String("routerd.log.sequence", strconv.FormatUint(entry.Sequence, 10)),
		log.String("routerd.producer.epoch", entry.ProducerEpoch), log.String("routerd.producer.sequence", strconv.FormatUint(entry.ProducerSequence, 10)),
	)
	// The logging API owns this dynamic boundary and supplies only JSON scalars.
	attrs := make([]log.KeyValue, 0, len(entry.Attributes))
	for key, value := range entry.Attributes {
		var v log.Value
		switch value := value.(type) {
		case string:
			v = log.StringValue(value)
		case bool:
			v = log.BoolValue(value)
		case float64:
			v = log.Float64Value(value)
		}
		attrs = append(attrs, log.KeyValue{Key: key, Value: v})
	}
	if len(attrs) > 0 {
		r.AddAttributes(log.Map("routerd.attributes", attrs...))
	}
	return r
}
