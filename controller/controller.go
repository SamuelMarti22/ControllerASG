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

type Actuator interface {
	ScaleUp(ctx context.Context) error
	ScaleDown(ctx context.Context) error
}

type Logger interface {
	LogError(err error)
	LogDecision(decision model.Decision, reason string, observed []model.ObservedMetric, currentCount int)
	LogActionResult(decision model.Decision, err error)
}

type Controller struct {
	config       *configuration.Config
	metrics      MetricsSource
	actuator     Actuator
	logger       Logger
	lastActionAt time.Time
	currentCount int
}

func New(config *configuration.Config, metrics MetricsSource, actuator Actuator, logger Logger, currentCount int) *Controller {
	return &Controller{
		config:       config,
		metrics:      metrics,
		actuator:     actuator,
		logger:       logger,
		currentCount: currentCount,
	}
}

func (c *Controller) Tick(ctx context.Context) {
	observed, err := c.metrics.Observe(ctx)
	if err != nil {
		c.logger.LogError(err) // fail-safe: si no hay métricas confiables, NO actúes
		return
	}

	decision, reason := c.decide(observed)
	c.logger.LogDecision(decision, reason, observed, c.currentCount)

	switch decision {
	case model.Increase:
		if err := c.actuator.ScaleUp(ctx); err != nil {
			c.logger.LogActionResult(decision, err)
			return
		}
		c.logger.LogActionResult(decision, nil)
		c.currentCount++
		c.lastActionAt = time.Now()
	case model.Reduce:
		if err := c.actuator.ScaleDown(ctx); err != nil {
			c.logger.LogActionResult(decision, err)
			return
		}
		c.logger.LogActionResult(decision, nil)
		c.currentCount--
		c.lastActionAt = time.Now()
	}
}

func (c *Controller) decide(observed []model.ObservedMetric) (model.Decision, string) {
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
	case anyAboveUp && c.currentCount < c.config.Compute.MaxInstances:
		return model.Increase, "al menos una métrica superó su umbral de subida"
	case allBelowDown && c.currentCount > c.config.Compute.MinInstances:
		return model.Reduce, "todas las métricas están por debajo de su umbral de bajada"
	default:
		return model.Maintain, "condiciones estables, métricas incompletas o en límites de min/max"
	}
}

func toMap(observed []model.ObservedMetric) map[string]float64 {
	m := make(map[string]float64, len(observed))
	for _, o := range observed {
		m[o.Name] = o.Value
	}
	return m
}
