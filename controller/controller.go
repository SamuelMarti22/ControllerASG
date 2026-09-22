package controller

import (
	"context"
	"time"

	"controllerasg/configuration"
	"controllerasg/model"
)

type MetricsSource interface {
	Observe(ctx context.Context) ([]model.ObservedMetric, error)
}

// Fleet informa de las instancias gestionadas y su salud: es la fuente de verdad
// de la capacidad.
type Fleet interface {
	Instances(ctx context.Context) ([]model.Instance, error)
}

// Actuator devuelve el id de la instancia creada o retirada.
type Actuator interface {
	ScaleUp(ctx context.Context) (string, error)
	ScaleDown(ctx context.Context) (string, error)
}

type Logger interface {
	Log(rec model.DecisionRecord)
}

type Controller struct {
	config       *configuration.Config
	metrics      MetricsSource
	fleet        Fleet
	actuator     Actuator
	logger       Logger
	lastActionAt time.Time
}

func New(config *configuration.Config, metrics MetricsSource, fleet Fleet, actuator Actuator, logger Logger) *Controller {
	return &Controller{config: config, metrics: metrics, fleet: fleet, actuator: actuator, logger: logger}
}

// Tick ejecuta un ciclo observar → decidir → actuar y registra exactamente un
// DecisionRecord, incluso cuando algo falla.
func (c *Controller) Tick(ctx context.Context) {
	rec := model.DecisionRecord{
		Time:          time.Now(),
		Decision:      model.Maintain,
		WindowSeconds: c.config.ObservationWindowSeconds,
		Action:        "none",
		Result:        "none",
	}
	defer func() { c.logger.Log(rec) }()

	fleet, err := c.fleet.Instances(ctx)
	if err != nil {
		// sin saber cuánta capacidad hay no se puede actuar con seguridad
		rec.Reason = "no se pudo leer la flota, no se actúa"
		rec.Error = err.Error()
		return
	}
	rec.Capacity = summarize(fleet)

	observed, obsErr := c.metrics.Observe(ctx)
	if obsErr != nil {
		observed = nil
		rec.Error = "observación: " + obsErr.Error()
	}
	rec.Metrics = observed

	rec.Decision, rec.Reason = c.decide(observed, len(fleet))
	if obsErr != nil && rec.Decision == model.Maintain {
		rec.Reason = "métricas no disponibles, no se actúa"
	}

	switch rec.Decision {
	case model.Increase:
		rec.Action = "ScaleUp"
		rec.InstanceID, err = c.actuator.ScaleUp(ctx)
	case model.Reduce:
		rec.Action = "ScaleDown"
		rec.InstanceID, err = c.actuator.ScaleDown(ctx)
	default:
		return
	}
	if err != nil {
		rec.Result = "error"
		rec.Error = err.Error()
		return
	}
	rec.Result = "ok"
	c.lastActionAt = time.Now()
}

func (c *Controller) decide(observed []model.ObservedMetric, currentCount int) (model.Decision, string) {
	// El mínimo es una regla de seguridad: se restablece aunque no haya métricas
	// ni cooldown (arranque en frío, o una instancia que murió).
	if currentCount < c.config.Compute.MinInstances {
		return model.Increase, "capacidad por debajo del mínimo configurado"
	}

	if time.Since(c.lastActionAt) < time.Duration(c.config.CooldownSeconds)*time.Second {
		return model.Maintain, "en cooldown"
	}

	if len(observed) == 0 {
		return model.Maintain, "sin datos de métricas, no se actúa"
	}

	values := toMap(observed) // metricName -> valor

	anyAboveUp := false
	allBelowDown := true

	for _, policy := range c.config.Metrics {
		v, ok := values[policy.MetricName]
		if !ok {
			// sin dato no se puede afirmar que sea seguro reducir capacidad,
			// pero tampoco bloquea un aumento por otra métrica
			allBelowDown = false
			continue
		}
		if v >= policy.Scaling.ScaleUpThreshold {
			anyAboveUp = true
		}
		if v > policy.Scaling.ScaleDownThreshold {
			allBelowDown = false
		}
	}

	switch {
	case anyAboveUp && currentCount < c.config.Compute.MaxInstances:
		return model.Increase, "al menos una métrica superó su umbral de subida"
	case allBelowDown && currentCount > c.config.Compute.MinInstances:
		return model.Reduce, "todas las métricas están por debajo de su umbral de bajada"
	default:
		return model.Maintain, "condiciones estables, métricas incompletas o en límites de min/max"
	}
}

func summarize(fleet []model.Instance) model.Capacity {
	cap := model.Capacity{Total: len(fleet), Instances: make([]model.InstanceStatus, 0, len(fleet))}
	for _, i := range fleet {
		if i.Health == "healthy" {
			cap.Healthy++
		}
		cap.Instances = append(cap.Instances, model.InstanceStatus{ID: i.Id, State: i.State, Health: i.Health})
	}
	return cap
}

func toMap(observed []model.ObservedMetric) map[string]float64 {
	m := make(map[string]float64, len(observed))
	for _, o := range observed {
		m[o.Name] = o.Value
	}
	return m
}
