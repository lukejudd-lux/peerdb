package otel_metrics

import (
	"context"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/embedded"
)

type ObservationMapValue[V comparable] struct {
	Value V
}

// SyncGauge is a generic synchronous gauge that can be used to observe any type of value
// Inspired from https://github.com/open-telemetry/opentelemetry-go/issues/3984#issuecomment-1743231837
type SyncGauge[V comparable, O metric.Observable] struct {
	observableGauge O
	observations    sync.Map
	name            string
}

func (a *SyncGauge[V, O]) Callback(ctx context.Context, observeFunc func(value V, options ...metric.ObserveOption)) error {
	a.observations.Range(func(key, value interface{}) bool {
		attrs := key.(attribute.Set)
		val := value.(*ObservationMapValue[V])
		observeFunc(val.Value, metric.WithAttributeSet(attrs))
		// If the pointer is still same we can safely delete, else it means that the value was overwritten in parallel
		a.observations.CompareAndDelete(attrs, val)
		return true
	})
	return nil
}

func (a *SyncGauge[V, O]) Record(input V, attrs attribute.Set) {
	val := ObservationMapValue[V]{Value: input}
	a.observations.Store(attrs, &val)
}

type Int64SyncGauge struct {
	embedded.Int64Gauge
	syncGauge *SyncGauge[int64, metric.Int64ObservableGauge]
}

func (a *Int64SyncGauge) Record(ctx context.Context, value int64, options ...metric.RecordOption) {
	if a == nil {
		return
	}
	c := metric.NewRecordConfig(options)
	a.syncGauge.Record(value, c.Attributes())
}

func NewInt64SyncGauge(meter metric.Meter, gaugeName string, opts ...metric.Int64ObservableGaugeOption) (*Int64SyncGauge, error) {
	syncGauge := &SyncGauge[int64, metric.Int64ObservableGauge]{
		name: gaugeName,
	}
	observableGauge, err := meter.Int64ObservableGauge(gaugeName,
		append(opts, metric.WithInt64Callback(func(ctx context.Context, observer metric.Int64Observer) error {
			return syncGauge.Callback(ctx, func(value int64, options ...metric.ObserveOption) {
				observer.Observe(value, options...)
			})
		}))...)
	if err != nil {
		return nil, fmt.Errorf("failed to create Int64SyncGauge: %w", err)
	}
	syncGauge.observableGauge = observableGauge
	return &Int64SyncGauge{syncGauge: syncGauge}, nil
}

type Float64SyncGauge struct {
	embedded.Float64Gauge
	syncGauge *SyncGauge[float64, metric.Float64Observable]
}

func (a *Float64SyncGauge) Record(ctx context.Context, value float64, options ...metric.RecordOption) {
	if a == nil {
		return
	}
	c := metric.NewRecordConfig(options)
	a.syncGauge.Record(value, c.Attributes())
}

func NewFloat64SyncGauge(meter metric.Meter, gaugeName string, opts ...metric.Float64ObservableGaugeOption) (*Float64SyncGauge, error) {
	syncGauge := &SyncGauge[float64, metric.Float64Observable]{
		name: gaugeName,
	}
	observableGauge, err := meter.Float64ObservableGauge(gaugeName,
		append(opts, metric.WithFloat64Callback(func(ctx context.Context, observer metric.Float64Observer) error {
			return syncGauge.Callback(ctx, func(value float64, options ...metric.ObserveOption) {
				observer.Observe(value, options...)
			})
		}))...)
	if err != nil {
		return nil, fmt.Errorf("failed to create Float64SyncGauge: %w", err)
	}
	syncGauge.observableGauge = observableGauge
	return &Float64SyncGauge{syncGauge: syncGauge}, nil
}

func Int64Gauge(meter metric.Meter, name string, opts ...metric.Int64GaugeOption) (metric.Int64Gauge, error) {
	gaugeConfig := metric.NewInt64GaugeConfig(opts...)
	return NewInt64SyncGauge(meter, name,
		metric.WithDescription(gaugeConfig.Description()),
		metric.WithUnit(gaugeConfig.Unit()),
	)
}

func Float64Gauge(meter metric.Meter, name string, opts ...metric.Float64GaugeOption) (metric.Float64Gauge, error) {
	gaugeConfig := metric.NewFloat64GaugeConfig(opts...)
	return NewFloat64SyncGauge(meter, name,
		metric.WithDescription(gaugeConfig.Description()),
		metric.WithUnit(gaugeConfig.Unit()),
	)
}
