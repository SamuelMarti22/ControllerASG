//go:build integration

package provisioner

// Prueba contra AWS real (crea y termina una instancia EC2). Requiere la
// infraestructura de infra/up.sh, credenciales AWS y un conf.json con sus IDs.
//
//	go test -count=1 -tags integration -v -run TestIntegration -timeout 15m ./provisioner/

import (
	"context"
	"os"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	elbv2 "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"

	"controllerasg/configuration"
)

func TestIntegrationScaleUpAndDown(t *testing.T) {
	path := os.Getenv("CONFIG_PATH")
	if path == "" {
		path = "../conf.json"
	}
	cfg, err := configuration.Load(path)
	if err != nil {
		t.Skipf("sin config válida (%v)", err)
	}
	cfg.Compute.MinInstances = 0 // la prueba parte de una flota vacía

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.AWS.Region))
	if err != nil {
		t.Fatal(err)
	}
	ec2c, elbc := ec2.NewFromConfig(awsCfg), elbv2.NewFromConfig(awsCfg)
	p := New(ec2c, elbc, cfg)

	before, err := p.Instances(ctx)
	if err != nil {
		t.Fatalf("Instances: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("se esperaba flota vacía, hay %d instancias gestionadas", len(before))
	}

	// Limpieza de seguridad: si algo falla a mitad, no dejar instancias vivas.
	t.Cleanup(func() {
		c, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		left, _ := p.Instances(c)
		for _, i := range left {
			t.Logf("limpieza: terminando %s", i.Id)
			ec2c.TerminateInstances(c, &ec2.TerminateInstancesInput{InstanceIds: []string{i.Id}})
		}
	})

	start := time.Now()
	id, err := p.ScaleUp(ctx)
	if err != nil {
		t.Fatalf("ScaleUp: %v", err)
	}
	t.Logf("ScaleUp completado en %s (%s en running y registrada)", time.Since(start).Round(time.Second), id)

	// running no es lo mismo que disponible: esperar al health check del target group.
	var healthyAfter time.Duration
	for {
		fleet, err := p.Instances(ctx)
		if err != nil {
			t.Fatalf("Instances: %v", err)
		}
		if len(fleet) != 1 {
			t.Fatalf("se esperaba 1 instancia, hay %d", len(fleet))
		}
		t.Logf("  %s estado=%s salud=%s subred=%s (t+%s)", fleet[0].Id, fleet[0].State, fleet[0].Health, fleet[0].SubnetID, time.Since(start).Round(time.Second))
		if fleet[0].Health == "healthy" {
			healthyAfter = time.Since(start)
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("la instancia no llegó a healthy a tiempo")
		case <-time.After(10 * time.Second):
		}
	}
	t.Logf("Tiempo desde la orden hasta capacidad realmente disponible: %s", healthyAfter.Round(time.Second))

	if _, err := p.ScaleDown(ctx); err != nil {
		t.Fatalf("ScaleDown: %v", err)
	}
	t.Log("ScaleDown completado (desregistrada y orden de terminación enviada)")

	// EC2 tarda unos segundos en reflejar el cambio de estado tras TerminateInstances.
	deadline := time.Now().Add(time.Minute)
	for {
		fleet, err := p.Instances(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(fleet) == 0 {
			t.Log("la flota gestionada quedó vacía")
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("la instancia %s sigue %s 1 min después de ScaleDown", fleet[0].Id, fleet[0].State)
		}
		time.Sleep(5 * time.Second)
	}
}
