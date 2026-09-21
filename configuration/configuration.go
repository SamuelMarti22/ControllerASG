package configuration

import (
	"encoding/json"
	"fmt"
	"os"
)

type AWSConfig struct {
	Region string
}

type ComputeConfig struct {
	LaunchTemplateID      string
	LaunchTemplateVersion string
	MinInstances          int
	MaxInstances          int
	SubnetIDs             []string
	SecurityGroupIDs      []string
}

type LoadBalancerConfig struct {
	TargetGroupARN string
	TargetPort     int
}

type ScalingConfig struct {
	ScaleUpThreshold   float64
	ScaleDownThreshold float64
}

type Dimension struct {
	Name  string
	Value string
}

type MetricPolicy struct {
	Namespace  string
	MetricName string
	Dimensions []Dimension
	Period     int
	Stat       string
	Scaling    ScalingConfig
}

type DeploymentConfig struct {
	UseDocker bool
}

type Config struct {
	AWS                      AWSConfig
	Compute                  ComputeConfig
	LoadBalancer             LoadBalancerConfig
	PollIntervalSeconds      int
	ObservationWindowSeconds int
	CooldownSeconds          int
	Metrics                  []MetricPolicy
	Deployment               DeploymentConfig
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("leyendo config: %w", err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parseando config: %w", err)
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("config inválida: %w", err)
	}

	return &config, nil
}

func (config *Config) Validate() error {
	if config.AWS.Region == "" {
		return fmt.Errorf("aws.region es obligatorio")
	}
	if config.Compute.LaunchTemplateID == "" {
		return fmt.Errorf("compute.launchTemplateId es obligatorio")
	}
	if config.LoadBalancer.TargetGroupARN == "" {
		return fmt.Errorf("loadBalancer.targetGroupArn es obligatorio")
	}
	if len(config.Compute.SubnetIDs) == 0 {
		return fmt.Errorf("compute.subnetIds necesita al menos una subred")
	}
	if config.Compute.MinInstances < 1 {
		return fmt.Errorf("minInstances debe ser al menos 1")
	}
	if config.Compute.MaxInstances > 5 {
		return fmt.Errorf("maxInstances debe ser como máximo 5")
	}
	if config.Compute.MinInstances > config.Compute.MaxInstances {
		return fmt.Errorf("minInstances debe ser menor o igual a maxInstances")
	}
	if config.PollIntervalSeconds <= 0 {
		return fmt.Errorf("pollIntervalSeconds debe ser mayor que 0")
	}
	if config.CooldownSeconds < 0 {
		return fmt.Errorf("cooldownSeconds no puede ser negativo")
	}
	if len(config.Metrics) == 0 {
		return fmt.Errorf("se necesita al menos 1 política de métrica")
	}

	for _, m := range config.Metrics {
		if m.Namespace == "" || m.MetricName == "" || m.Stat == "" {
			return fmt.Errorf("métrica %q: namespace, metricName y stat son obligatorios", m.MetricName)
		}
		if m.Period <= 0 || m.Period%60 != 0 {
			return fmt.Errorf("métrica %q: period debe ser un múltiplo positivo de 60", m.MetricName)
		}
		if config.ObservationWindowSeconds < m.Period {
			return fmt.Errorf("observationWindowSeconds debe ser al menos el period de %q", m.MetricName)
		}
		if m.Scaling.ScaleUpThreshold <= m.Scaling.ScaleDownThreshold {
			return fmt.Errorf("métrica %q: scaleUpThreshold debe ser mayor que scaleDownThreshold", m.MetricName)
		}
	}

	return nil
}
