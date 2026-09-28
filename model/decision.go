package model

import "time"

type Decision string

const (
	Maintain Decision = "MAINTAIN_CAPACITY"
	Increase Decision = "INCREASE_CAPACITY"
	Reduce   Decision = "REDUCE_CAPACITY"
)

// InstanceStatus es la vista compacta de una instancia que se guarda en el log.
type InstanceStatus struct {
	ID     string
	State  string
	Health string
}

type Capacity struct {
	Total     int
	Healthy   int
	Instances []InstanceStatus
}

// DecisionRecord es una línea del log de decisiones: permite reconstruir qué se
// observó, qué se decidió y por qué, qué se pidió a la infraestructura y qué pasó.
type DecisionRecord struct {
	Time          time.Time
	Decision      Decision
	Reason        string
	WindowSeconds int
	Metrics       []ObservedMetric
	Capacity      Capacity
	InstanceID    string
	Action        string // instancia creada o retirada
	Result        string // none | ScaleUp | ScaleDown
	Error         string // none | ok | error
}
