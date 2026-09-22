package model

import "time"

type ObservedMetric struct {
	Name      string
	Value     float64
	Timestamp time.Time `json:"timestamp"`
}
