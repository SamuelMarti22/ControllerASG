package model

import "time"

type Instance struct {
	Id            string
	Ip            [4]uint8
	SubnetID      string
	State         string // estado EC2: pending, running...
	Health        string // salud en el target group: healthy, initial, unhealthy, unregistered...
	LaunchTime    time.Time
	ActualMetrics []ObservedMetric
}

type GroupInstances struct {
	Instances []Instance
}
