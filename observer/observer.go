package observer

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"controllerasg/configuration"
	"controllerasg/model"
)

type CloudWatchObserver struct {
	client  cloudwatch.GetMetricDataAPIClient
	metrics []configuration.MetricPolicy
	window  time.Duration
}

// series agrupa los datapoints crudos que CloudWatch devolvió para una consulta.
type series struct {
	values []float64
	newest time.Time
}

func New(client cloudwatch.GetMetricDataAPIClient, metrics []configuration.MetricPolicy, window time.Duration) *CloudWatchObserver {
	return &CloudWatchObserver{client: client, metrics: metrics, window: window}
}

// Observe consulta todas las métricas y devuelve, por cada una, un valor
// ponderado de la ventana de observación. Las métricas sin datos se omiten
// (no se reportan como 0): el controller decide qué hacer con la información
// incompleta.
func (observer *CloudWatchObserver) Observe(ctx context.Context) ([]model.ObservedMetric, error) {
	queries := observer.buildQueries()

	raw, err := observer.fetchSeries(ctx, queries)
	if err != nil {
		return nil, err
	}

	return observer.average(raw), nil
}

func queryID(i int) string {
	return fmt.Sprintf("m%d", i) // el id debe empezar con minúscula
}

// buildQueries traduce cada política de métrica del config a una consulta de GetMetricData.
func (observer *CloudWatchObserver) buildQueries() []types.MetricDataQuery {
	queries := make([]types.MetricDataQuery, 0, len(observer.metrics))
	for i, m := range observer.metrics {
		dimensions := make([]types.Dimension, 0, len(m.Dimensions))
		for _, d := range m.Dimensions {
			dimensions = append(dimensions, types.Dimension{Name: aws.String(d.Name), Value: aws.String(d.Value)})
		}
		queries = append(queries, types.MetricDataQuery{
			Id: aws.String(queryID(i)),
			MetricStat: &types.MetricStat{
				Metric: &types.Metric{
					Namespace:  aws.String(m.Namespace),
					MetricName: aws.String(m.MetricName),
					Dimensions: dimensions,
				},
				Period: aws.Int32(int32(m.Period)),
				Stat:   aws.String(m.Stat),
			},
		})
	}
	return queries
}

// fetchSeries ejecuta las consultas en un solo GetMetricData (con paginación)
// y devuelve los datapoints crudos indexados por id de consulta.
func (observer *CloudWatchObserver) fetchSeries(ctx context.Context, queries []types.MetricDataQuery) (map[string]*series, error) {
	now := time.Now()
	raw := make(map[string]*series)

	p := cloudwatch.NewGetMetricDataPaginator(observer.client, &cloudwatch.GetMetricDataInput{
		MetricDataQueries: queries,
		StartTime:         aws.Time(now.Add(-observer.window)),
		EndTime:           aws.Time(now),
		ScanBy:            types.ScanByTimestampDescending,
	})
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("GetMetricData: %w", err)
		}
		for _, r := range out.MetricDataResults {
			id := aws.ToString(r.Id)
			if r.StatusCode == types.StatusCodeForbidden {
				return nil, fmt.Errorf("GetMetricData: acceso denegado a la consulta %s", id)
			}
			s, ok := raw[id]
			if !ok {
				s = &series{}
				raw[id] = s
			}
			s.values = append(s.values, r.Values...)
			for _, ts := range r.Timestamps {
				if ts.After(s.newest) {
					s.newest = ts
				}
			}
		}
	}
	return raw, nil
}

// average reduce los datapoints de cada métrica a un solo valor (promedio de
// la ventana). Las métricas sin datapoints se omiten.
func (observer *CloudWatchObserver) average(raw map[string]*series) []model.ObservedMetric {
	var result []model.ObservedMetric
	for i, m := range observer.metrics {
		s, ok := raw[queryID(i)]
		if !ok || len(s.values) == 0 {
			continue
		}
		var sum float64
		for _, v := range s.values {
			sum += v
		}
		result = append(result, model.ObservedMetric{
			Name:      m.MetricName,
			Value:     sum / float64(len(s.values)),
			Timestamp: s.newest,
		})
	}
	return result
}
