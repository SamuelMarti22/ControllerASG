package observer

import (
	"context"
	"fmt"
	"strings"
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
// ponderado de la ventana de observación. Las métricas "por instancia" (p. ej.
// CPU) se consultan una vez por cada instancia de fleet y se promedian entre
// sí. Las métricas sin datos se omiten (no se reportan como 0): el controller
// decide qué hacer con la información incompleta.
func (observer *CloudWatchObserver) Observe(ctx context.Context, fleet []model.Instance) ([]model.ObservedMetric, error) {
	queries, ids := observer.buildQueries(fleet)

	raw, err := observer.fetchSeries(ctx, queries)
	if err != nil {
		return nil, err
	}

	return observer.aggregate(raw, ids), nil
}

func queryID(i int) string {
	return fmt.Sprintf("m%d", i) // el id debe empezar con minúscula
}

// instanceQueryID identifica la consulta de la política i para una instancia
// concreta. El id de CloudWatch solo admite letras, dígitos y '_', por eso se
// quita el guion de "i-xxxx".
func instanceQueryID(i int, instanceID string) string {
	return fmt.Sprintf("m%d_%s", i, strings.ReplaceAll(instanceID, "-", ""))
}

// buildQueries traduce cada política de métrica del config a una o varias
// consultas de GetMetricData, y devuelve además, por política, la lista de
// ids que hay que promediar para obtener su valor final.
func (observer *CloudWatchObserver) buildQueries(fleet []model.Instance) ([]types.MetricDataQuery, map[int][]string) {
	var queries []types.MetricDataQuery
	ids := make(map[int][]string, len(observer.metrics))

	for i, m := range observer.metrics {
		if m.PerInstance {
			for _, inst := range fleet {
				id := instanceQueryID(i, inst.Id)
				ids[i] = append(ids[i], id)
				queries = append(queries, metricQuery(id, m.Namespace, m.MetricName, m.Period, m.Stat,
					[]types.Dimension{{Name: aws.String("InstanceId"), Value: aws.String(inst.Id)}}))
			}
			continue
		}

		dimensions := make([]types.Dimension, 0, len(m.Dimensions))
		for _, d := range m.Dimensions {
			dimensions = append(dimensions, types.Dimension{Name: aws.String(d.Name), Value: aws.String(d.Value)})
		}
		id := queryID(i)
		ids[i] = []string{id}
		queries = append(queries, metricQuery(id, m.Namespace, m.MetricName, m.Period, m.Stat, dimensions))
	}
	return queries, ids
}

func metricQuery(id, namespace, metricName string, period int, stat string, dimensions []types.Dimension) types.MetricDataQuery {
	return types.MetricDataQuery{
		Id: aws.String(id),
		MetricStat: &types.MetricStat{
			Metric: &types.Metric{
				Namespace:  aws.String(namespace),
				MetricName: aws.String(metricName),
				Dimensions: dimensions,
			},
			Period: aws.Int32(int32(period)),
			Stat:   aws.String(stat),
		},
	}
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

// windowAverage promedia los datapoints de una consulta en la ventana de
// observación. ok=false si no llegó ningún datapoint.
func windowAverage(s *series) (avg float64, ok bool) {
	if s == nil || len(s.values) == 0 {
		return 0, false
	}
	var sum float64
	for _, v := range s.values {
		sum += v
	}
	return sum / float64(len(s.values)), true
}

// aggregate reduce cada política a un solo valor: el promedio de la ventana
// de cada una de sus consultas (una para una métrica normal, una por
// instancia para una métrica PerInstance) promediado a su vez entre ellas.
// Una política sin ningún dato se omite (no se reporta como 0).
func (observer *CloudWatchObserver) aggregate(raw map[string]*series, ids map[int][]string) []model.ObservedMetric {
	var result []model.ObservedMetric
	for i, m := range observer.metrics {
		var sum float64
		var n int
		var newest time.Time
		for _, id := range ids[i] {
			avg, ok := windowAverage(raw[id])
			if !ok {
				continue
			}
			sum += avg
			n++
			if s := raw[id]; s.newest.After(newest) {
				newest = s.newest
			}
		}
		if n == 0 {
			continue
		}
		result = append(result, model.ObservedMetric{Name: m.MetricName, Value: sum / float64(n), Timestamp: newest})
	}
	return result
}
