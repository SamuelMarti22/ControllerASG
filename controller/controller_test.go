package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"controllerasg/configuration"
	"controllerasg/model"
)

type fakeMetrics struct {
	values []model.ObservedMetric
	err    error
}

func (f fakeMetrics) Observe(context.Context) ([]model.ObservedMetric, error) { return f.values, f.err }

type fakeFleet struct {
	instances []model.Instance
	err       error
}

func (f fakeFleet) Instances(context.Context) ([]model.Instance, error) { return f.instances, f.err }

type fakeActuator struct {
	ups, downs int
	err        error
}

func (f *fakeActuator) ScaleUp(context.Context) (string, error) {
	f.ups++
	return "i-new", f.err
}

func (f *fakeActuator) ScaleDown(context.Context) (string, error) {
	f.downs++
	return "i-old", f.err
}

type memLogger struct{ recs []model.DecisionRecord }

func (l *memLogger) Log(r model.DecisionRecord) { l.recs = append(l.recs, r) }

func testConfig() *configuration.Config {
	return &configuration.Config{
		ObservationWindowSeconds: 300,
		CooldownSeconds:          300,
		Compute:                  configuration.ComputeConfig{MinInstances: 1, MaxInstances: 3},
		Metrics: []configuration.MetricPolicy{{
			MetricName: "load",
			Scaling:    configuration.ScalingConfig{ScaleUpThreshold: 70, ScaleDownThreshold: 20},
		}},
	}
}

func healthy(n int) []model.Instance {
	fleet := make([]model.Instance, n)
	for i := range fleet {
		fleet[i] = model.Instance{Id: "i-" + string(rune('a'+i)), State: "running", Health: "healthy"}
	}
	return fleet
}

func run(m fakeMetrics, f fakeFleet, a *fakeActuator) (*Controller, model.DecisionRecord) {
	l := &memLogger{}
	c := New(testConfig(), m, f, a, l)
	c.Tick(context.Background())
	return c, l.recs[0]
}

func load(v float64) fakeMetrics {
	return fakeMetrics{values: []model.ObservedMetric{{Name: "load", Value: v}}}
}

func TestScaleUpOnHighLoad(t *testing.T) {
	a := &fakeActuator{}
	_, rec := run(load(90), fakeFleet{instances: healthy(1)}, a)
	if rec.Decision != model.Increase || a.ups != 1 || rec.Action != "ScaleUp" || rec.Result != "ok" || rec.InstanceID != "i-new" {
		t.Errorf("registro inesperado: %+v (ups=%d)", rec, a.ups)
	}
	if rec.Capacity.Total != 1 || rec.Capacity.Healthy != 1 || len(rec.Metrics) != 1 || rec.WindowSeconds != 300 {
		t.Errorf("el registro no describe lo observado: %+v", rec)
	}
}

func TestScaleDownOnLowLoad(t *testing.T) {
	a := &fakeActuator{}
	_, rec := run(load(5), fakeFleet{instances: healthy(2)}, a)
	if rec.Decision != model.Reduce || a.downs != 1 || rec.InstanceID != "i-old" {
		t.Errorf("registro inesperado: %+v", rec)
	}
}

func TestMaintainBetweenThresholds(t *testing.T) {
	a := &fakeActuator{}
	_, rec := run(load(50), fakeFleet{instances: healthy(2)}, a)
	if rec.Decision != model.Maintain || a.ups+a.downs != 0 || rec.Action != "none" {
		t.Errorf("registro inesperado: %+v", rec)
	}
}

func TestRespectsMinAndMax(t *testing.T) {
	a := &fakeActuator{}
	if _, rec := run(load(5), fakeFleet{instances: healthy(1)}, a); rec.Decision != model.Maintain {
		t.Errorf("no debía bajar del mínimo: %+v", rec)
	}
	if _, rec := run(load(99), fakeFleet{instances: healthy(3)}, a); rec.Decision != model.Maintain {
		t.Errorf("no debía superar el máximo: %+v", rec)
	}
	if a.ups+a.downs != 0 {
		t.Error("no debía actuar")
	}
}

func TestBelowMinScalesUpWithoutMetrics(t *testing.T) {
	a := &fakeActuator{}
	_, rec := run(fakeMetrics{err: errors.New("sin datos")}, fakeFleet{}, a)
	if rec.Decision != model.Increase || a.ups != 1 {
		t.Errorf("debía restablecer el mínimo: %+v", rec)
	}
	if rec.Error == "" {
		t.Error("debía registrar también el fallo de observación")
	}
}

func TestObserveErrorMaintains(t *testing.T) {
	a := &fakeActuator{}
	_, rec := run(fakeMetrics{err: errors.New("boom")}, fakeFleet{instances: healthy(2)}, a)
	if rec.Decision != model.Maintain || a.ups+a.downs != 0 || rec.Error == "" {
		t.Errorf("registro inesperado: %+v", rec)
	}
}

func TestNoMetricsNeverScalesDown(t *testing.T) {
	a := &fakeActuator{}
	_, rec := run(fakeMetrics{}, fakeFleet{instances: healthy(2)}, a)
	if rec.Decision != model.Maintain || a.downs != 0 {
		t.Errorf("registro inesperado: %+v", rec)
	}
}

func TestFleetErrorDoesNotAct(t *testing.T) {
	a := &fakeActuator{}
	_, rec := run(load(99), fakeFleet{err: errors.New("aws caído")}, a)
	if rec.Decision != model.Maintain || a.ups != 0 || rec.Error == "" {
		t.Errorf("registro inesperado: %+v", rec)
	}
}

func TestActuatorErrorIsRecorded(t *testing.T) {
	a := &fakeActuator{err: errors.New("InsufficientInstanceCapacity")}
	c, rec := run(load(99), fakeFleet{instances: healthy(1)}, a)
	if rec.Result != "error" || rec.Error == "" || rec.Decision != model.Increase {
		t.Errorf("registro inesperado: %+v", rec)
	}
	if !c.lastActionAt.IsZero() {
		t.Error("una acción fallida no debe iniciar el cooldown")
	}
}

func TestCooldownBlocksConsecutiveActions(t *testing.T) {
	a := &fakeActuator{}
	l := &memLogger{}
	c := New(testConfig(), load(99), fakeFleet{instances: healthy(1)}, a, l)
	c.Tick(context.Background())
	c.Tick(context.Background())
	if a.ups != 1 {
		t.Errorf("ups = %d, el cooldown debía bloquear la segunda", a.ups)
	}
	if l.recs[1].Reason != "en cooldown" {
		t.Errorf("motivo = %q", l.recs[1].Reason)
	}
	c.lastActionAt = time.Now().Add(-time.Hour)
	c.Tick(context.Background())
	if a.ups != 2 {
		t.Error("pasado el cooldown debía volver a actuar")
	}
}

func TestEveryTickLogsExactlyOneRecord(t *testing.T) {
	l := &memLogger{}
	c := New(testConfig(), fakeMetrics{err: errors.New("x")}, fakeFleet{err: errors.New("y")}, &fakeActuator{}, l)
	c.Tick(context.Background())
	if len(l.recs) != 1 {
		t.Errorf("registros = %d", len(l.recs))
	}
}
