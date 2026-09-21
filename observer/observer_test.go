package observer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"controllerasg/configuration"
)

// fakeClient devuelve páginas predefinidas, una por llamada.
type fakeClient struct {
	pages []*cloudwatch.GetMetricDataOutput
	err   error
	calls int
	last  *cloudwatch.GetMetricDataInput
}

func (f *fakeClient) GetMetricData(ctx context.Context, in *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	f.last = in
	if f.err != nil {
		return nil, f.err
	}
	page := f.pages[f.calls]
	f.calls++
	return page, nil
}

func testMetrics() []configuration.MetricPolicy {
	dims := []configuration.Dimension{{Name: "TargetGroup", Value: "targetgroup/tg/1"}}
	return []configuration.MetricPolicy{
		{Namespace: "AWS/ApplicationELB", MetricName: "RequestCountPerTarget", Dimensions: dims, Period: 60, Stat: "Sum"},
		{Namespace: "AWS/ApplicationELB", MetricName: "TargetResponseTime", Dimensions: dims, Period: 60, Stat: "Average"},
	}
}

func result(id string, values ...float64) types.MetricDataResult {
	ts := make([]time.Time, len(values))
	base := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	for i := range values {
		ts[i] = base.Add(-time.Duration(i) * time.Minute) // el más nuevo primero
	}
	return types.MetricDataResult{Id: aws.String(id), Values: values, Timestamps: ts, StatusCode: types.StatusCodeComplete}
}

func TestBuildQueries(t *testing.T) {
	o := New(&fakeClient{}, testMetrics(), 5*time.Minute)
	qs := o.buildQueries()

	if len(qs) != 2 {
		t.Fatalf("esperaba 2 queries, obtuve %d", len(qs))
	}
	q := qs[1]
	if aws.ToString(q.Id) != "m1" {
		t.Errorf("id = %s, esperaba m1", aws.ToString(q.Id))
	}
	m := q.MetricStat.Metric
	if aws.ToString(m.Namespace) != "AWS/ApplicationELB" || aws.ToString(m.MetricName) != "TargetResponseTime" {
		t.Errorf("namespace/nombre incorrectos: %s %s", aws.ToString(m.Namespace), aws.ToString(m.MetricName))
	}
	if len(m.Dimensions) != 1 || aws.ToString(m.Dimensions[0].Name) != "TargetGroup" {
		t.Errorf("dimensiones incorrectas: %+v", m.Dimensions)
	}
	if aws.ToInt32(q.MetricStat.Period) != 60 || aws.ToString(q.MetricStat.Stat) != "Average" {
		t.Errorf("period/stat incorrectos")
	}
}

func TestObserveAveragesWindow(t *testing.T) {
	client := &fakeClient{pages: []*cloudwatch.GetMetricDataOutput{{
		MetricDataResults: []types.MetricDataResult{
			result("m0", 100, 200, 300),
			result("m1", 0.2, 0.4),
		},
	}}}
	o := New(client, testMetrics(), 5*time.Minute)

	got, err := o.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("esperaba 2 métricas, obtuve %d", len(got))
	}
	if got[0].Name != "RequestCountPerTarget" || got[0].Value != 200 {
		t.Errorf("m0 = %+v, esperaba promedio 200", got[0])
	}
	if got[1].Value < 0.29 || got[1].Value > 0.31 {
		t.Errorf("m1 = %v, esperaba ~0.3", got[1].Value)
	}
	if got[0].Timestamp.IsZero() {
		t.Error("timestamp no debería ser cero")
	}
	if window := client.last.EndTime.Sub(*client.last.StartTime); window != 5*time.Minute {
		t.Errorf("ventana = %v, esperaba 5m", window)
	}
}

func TestObserveOmitsMetricsWithoutData(t *testing.T) {
	client := &fakeClient{pages: []*cloudwatch.GetMetricDataOutput{{
		MetricDataResults: []types.MetricDataResult{
			result("m0", 100),
			result("m1"), // sin datapoints
		},
	}}}
	o := New(client, testMetrics(), 5*time.Minute)

	got, err := o.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "RequestCountPerTarget" {
		t.Errorf("esperaba solo RequestCountPerTarget, obtuve %+v", got)
	}
}

func TestObserveNoDataAtAll(t *testing.T) {
	client := &fakeClient{pages: []*cloudwatch.GetMetricDataOutput{{}}}
	o := New(client, testMetrics(), 5*time.Minute)

	got, err := o.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("esperaba lista vacía, obtuve %+v", got)
	}
}

func TestObserveMergesPages(t *testing.T) {
	client := &fakeClient{pages: []*cloudwatch.GetMetricDataOutput{
		{MetricDataResults: []types.MetricDataResult{result("m0", 100)}, NextToken: aws.String("siguiente")},
		{MetricDataResults: []types.MetricDataResult{result("m0", 300)}},
	}}
	o := New(client, testMetrics()[:1], 5*time.Minute)

	got, err := o.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if client.calls != 2 {
		t.Errorf("llamadas = %d, esperaba 2", client.calls)
	}
	if len(got) != 1 || got[0].Value != 200 {
		t.Errorf("esperaba promedio 200 entre páginas, obtuve %+v", got)
	}
}

func TestObservePropagatesAWSError(t *testing.T) {
	boom := errors.New("throttled")
	o := New(&fakeClient{err: boom}, testMetrics(), 5*time.Minute)

	got, err := o.Observe(context.Background())
	if !errors.Is(err, boom) {
		t.Errorf("esperaba error envuelto, obtuve %v", err)
	}
	if got != nil {
		t.Errorf("con error no debe devolver métricas, obtuve %+v", got)
	}
}

func TestObserveForbidden(t *testing.T) {
	forbidden := result("m0", 100)
	forbidden.StatusCode = types.StatusCodeForbidden
	client := &fakeClient{pages: []*cloudwatch.GetMetricDataOutput{{
		MetricDataResults: []types.MetricDataResult{forbidden},
	}}}
	o := New(client, testMetrics(), 5*time.Minute)

	if _, err := o.Observe(context.Background()); err == nil {
		t.Error("esperaba error por acceso denegado")
	}
}
