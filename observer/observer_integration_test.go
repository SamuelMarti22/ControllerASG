//go:build integration

package observer

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"

	"controllerasg/configuration"
	"controllerasg/model"
)

// Prueba contra CloudWatch real. Ejecutar con:
//
//	TEST_INSTANCE_ID=i-xxxx go test -tags integration -v -run TestIntegration ./observer/
//
// Variables opcionales:
//
//	AWS_REGION       (por defecto us-east-1)
//	TEST_PERIOD      período en segundos (por defecto 300; usa 60 si la instancia tiene detailed monitoring)
//	TEST_WINDOW_MIN  ventana en minutos (por defecto 15)
func TestIntegrationEC2CPU(t *testing.T) {
	instanceID := os.Getenv("TEST_INSTANCE_ID")
	if instanceID == "" {
		t.Skip("define TEST_INSTANCE_ID para correr esta prueba")
	}

	region := envOr("AWS_REGION", "us-east-1")
	period, _ := strconv.Atoi(envOr("TEST_PERIOD", "300"))
	windowMin, _ := strconv.Atoi(envOr("TEST_WINDOW_MIN", "15"))

	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		t.Fatalf("cargando credenciales: %v", err)
	}

	metrics := []configuration.MetricPolicy{{
		Namespace:  "AWS/EC2",
		MetricName: "CPUUtilization",
		Dimensions: []configuration.Dimension{{Name: "InstanceId", Value: instanceID}},
		Period:     period,
		Stat:       "Average",
	}}

	o := New(cloudwatch.NewFromConfig(cfg), metrics, time.Duration(windowMin)*time.Minute)

	got, err := o.Observe(ctx, nil)
	if err != nil {
		t.Fatalf("Observe falló (¿credenciales o permisos cloudwatch:GetMetricData?): %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("CloudWatch respondió sin datos: la instancia debe llevar encendida unos minutos y period/ventana deben ser coherentes con el monitoreo")
	}
	for _, m := range got {
		t.Logf("%s = %.2f (dato más reciente: %s)", m.Name, m.Value, m.Timestamp.Format(time.RFC3339))
	}
}

// TestIntegrationPerInstanceCPU prueba el camino PerInstance: la misma
// instancia declarada en la flota, pero sin Dimensions en la política — el
// observer debe generar la dimensión InstanceId por su cuenta.
//
//	TEST_INSTANCE_ID=i-xxxx go test -tags integration -v -run TestIntegrationPerInstanceCPU ./observer/
func TestIntegrationPerInstanceCPU(t *testing.T) {
	instanceID := os.Getenv("TEST_INSTANCE_ID")
	if instanceID == "" {
		t.Skip("define TEST_INSTANCE_ID para correr esta prueba")
	}

	region := envOr("AWS_REGION", "us-east-1")
	period, _ := strconv.Atoi(envOr("TEST_PERIOD", "300"))
	windowMin, _ := strconv.Atoi(envOr("TEST_WINDOW_MIN", "15"))

	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		t.Fatalf("cargando credenciales: %v", err)
	}

	metrics := []configuration.MetricPolicy{{
		Namespace: "AWS/EC2", MetricName: "CPUUtilization", PerInstance: true, Period: period, Stat: "Average",
	}}
	o := New(cloudwatch.NewFromConfig(cfg), metrics, time.Duration(windowMin)*time.Minute)

	got, err := o.Observe(ctx, []model.Instance{{Id: instanceID}})
	if err != nil {
		t.Fatalf("Observe falló: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("CloudWatch respondió sin datos para la métrica per-instance")
	}
	t.Logf("%s (per-instance, 1 instancia) = %.2f", got[0].Name, got[0].Value)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
