package provisioner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	elbv2 "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"

	"controllerasg/configuration"
	"controllerasg/controller"
	"controllerasg/model"
)

var _ controller.Actuator = (*Provisioner)(nil)

type fakeEC2 struct {
	instances  []ec2types.Instance // flota gestionada que devuelve el listado
	runErr     error
	launched   *ec2.RunInstancesInput
	terminated []string
	events     *[]string
}

func (f *fakeEC2) RunInstances(ctx context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.launched = in
	if f.runErr != nil {
		return nil, f.runErr
	}
	return &ec2.RunInstancesOutput{Instances: []ec2types.Instance{{InstanceId: aws.String("i-new")}}}, nil
}

func (f *fakeEC2) TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.terminated = append(f.terminated, in.InstanceIds...)
	*f.events = append(*f.events, "terminate")
	return &ec2.TerminateInstancesOutput{}, nil
}

func (f *fakeEC2) DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if len(in.InstanceIds) > 0 { // consulta del waiter: la instancia ya está running
		return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{
			{InstanceId: aws.String(in.InstanceIds[0]), State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}},
		}}}}, nil
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: f.instances}}}, nil
}

type fakeELB struct {
	health       map[string]elbtypes.TargetHealthStateEnum
	registered   []string
	registerErr  error
	deregistered []string
	drainChecks  int
	events       *[]string
}

func (f *fakeELB) RegisterTargets(ctx context.Context, in *elbv2.RegisterTargetsInput, _ ...func(*elbv2.Options)) (*elbv2.RegisterTargetsOutput, error) {
	if f.registerErr != nil {
		return nil, f.registerErr
	}
	for _, t := range in.Targets {
		f.registered = append(f.registered, aws.ToString(t.Id))
	}
	return &elbv2.RegisterTargetsOutput{}, nil
}

func (f *fakeELB) DeregisterTargets(ctx context.Context, in *elbv2.DeregisterTargetsInput, _ ...func(*elbv2.Options)) (*elbv2.DeregisterTargetsOutput, error) {
	for _, t := range in.Targets {
		id := aws.ToString(t.Id)
		f.deregistered = append(f.deregistered, id)
		f.health[id] = elbtypes.TargetHealthStateEnumDraining
	}
	*f.events = append(*f.events, "deregister")
	return &elbv2.DeregisterTargetsOutput{}, nil
}

func (f *fakeELB) DescribeTargetHealth(ctx context.Context, in *elbv2.DescribeTargetHealthInput, _ ...func(*elbv2.Options)) (*elbv2.DescribeTargetHealthOutput, error) {
	out := &elbv2.DescribeTargetHealthOutput{}
	for id, state := range f.health {
		if state == elbtypes.TargetHealthStateEnumDraining {
			f.drainChecks++
			if f.drainChecks >= 2 { // el drenaje termina en la segunda consulta
				f.health[id] = elbtypes.TargetHealthStateEnumUnused
				state = elbtypes.TargetHealthStateEnumUnused
			}
		}
		out.TargetHealthDescriptions = append(out.TargetHealthDescriptions, elbtypes.TargetHealthDescription{
			Target:       &elbtypes.TargetDescription{Id: aws.String(id)},
			TargetHealth: &elbtypes.TargetHealth{State: state},
		})
	}
	return out, nil
}

func inst(id string, launched time.Time) ec2types.Instance {
	return ec2types.Instance{
		InstanceId:       aws.String(id),
		LaunchTime:       aws.Time(launched),
		PrivateIpAddress: aws.String("10.0.0.7"),
		State:            &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
	}
}

func setup(instances []ec2types.Instance, health map[string]elbtypes.TargetHealthStateEnum) (*Provisioner, *fakeEC2, *fakeELB, *[]string) {
	events := &[]string{}
	e := &fakeEC2{instances: instances, events: events}
	l := &fakeELB{health: health, events: events}
	cfg := &configuration.Config{
		Compute:      configuration.ComputeConfig{LaunchTemplateID: "lt-1", LaunchTemplateVersion: "$Latest", MinInstances: 1, MaxInstances: 3, SubnetIDs: []string{"subnet-a", "subnet-b"}},
		LoadBalancer: configuration.LoadBalancerConfig{TargetGroupARN: "arn:tg", TargetPort: 80},
	}
	p := New(e, l, cfg)
	p.drainPoll = time.Millisecond
	p.runningTimeout = time.Second
	return p, e, l, events
}

var t0 = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

func TestInstancesMergesHealthAndMarksUnregistered(t *testing.T) {
	p, _, _, _ := setup(
		[]ec2types.Instance{inst("i-a", t0), inst("i-b", t0)},
		map[string]elbtypes.TargetHealthStateEnum{"i-a": elbtypes.TargetHealthStateEnumHealthy},
	)
	fleet, err := p.Instances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, i := range fleet {
		got[i.Id] = i.Health
	}
	if got["i-a"] != "healthy" || got["i-b"] != "unregistered" {
		t.Errorf("salud = %v", got)
	}
	if fleet[0].Ip != [4]uint8{10, 0, 0, 7} || fleet[0].State != "running" {
		t.Errorf("ip/estado mal parseados: %+v", fleet[0])
	}
}

func TestScaleUpLaunchesTagsAndRegisters(t *testing.T) {
	p, e, l, _ := setup([]ec2types.Instance{inst("i-a", t0)}, map[string]elbtypes.TargetHealthStateEnum{"i-a": "healthy"})

	if err := p.ScaleUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(e.launched.LaunchTemplate.LaunchTemplateId) != "lt-1" {
		t.Error("no usó el launch template configurado")
	}
	tags := e.launched.TagSpecifications[0].Tags[0]
	if aws.ToString(tags.Key) != ManagedTagKey || aws.ToString(tags.Value) != ManagedTagValue {
		t.Errorf("tag incorrecto: %+v", tags)
	}
	if len(l.registered) != 1 || l.registered[0] != "i-new" {
		t.Errorf("registrados = %v", l.registered)
	}
	if aws.ToString(e.launched.SubnetId) != "subnet-a" {
		t.Errorf("subred = %s", aws.ToString(e.launched.SubnetId))
	}
}

func TestPickSubnetSpreadsAcrossZones(t *testing.T) {
	subnets := []string{"subnet-a", "subnet-b"}
	fleet := []model.Instance{{SubnetID: "subnet-a"}, {SubnetID: "subnet-a"}, {SubnetID: "subnet-b"}}
	if got := pickSubnet(subnets, fleet); got != "subnet-b" {
		t.Errorf("subred = %s, esperaba subnet-b", got)
	}
	if got := pickSubnet(subnets, nil); got != "subnet-a" {
		t.Errorf("sin flota debía usar la primera, usó %s", got)
	}
}

func TestScaleUpRefusesAtMax(t *testing.T) {
	fleet := []ec2types.Instance{inst("i-a", t0), inst("i-b", t0), inst("i-c", t0)}
	p, e, _, _ := setup(fleet, map[string]elbtypes.TargetHealthStateEnum{})

	if err := p.ScaleUp(context.Background()); err == nil {
		t.Fatal("esperaba error al superar el máximo")
	}
	if e.launched != nil {
		t.Error("no debía lanzar instancias")
	}
}

func TestScaleUpRollsBackWhenRegisterFails(t *testing.T) {
	p, e, l, _ := setup([]ec2types.Instance{inst("i-a", t0)}, map[string]elbtypes.TargetHealthStateEnum{})
	l.registerErr = errors.New("boom")

	if err := p.ScaleUp(context.Background()); err == nil {
		t.Fatal("esperaba error")
	}
	if len(e.terminated) != 1 || e.terminated[0] != "i-new" {
		t.Errorf("debía terminar la instancia huérfana, terminadas = %v", e.terminated)
	}
}

func TestScaleUpPropagatesRunError(t *testing.T) {
	p, e, l, _ := setup([]ec2types.Instance{inst("i-a", t0)}, map[string]elbtypes.TargetHealthStateEnum{})
	e.runErr = errors.New("InsufficientInstanceCapacity")

	if err := p.ScaleUp(context.Background()); !errors.Is(err, e.runErr) {
		t.Errorf("error = %v", err)
	}
	if len(l.registered) != 0 {
		t.Error("no debía registrar nada")
	}
}

func TestScaleDownDeregistersDrainsThenTerminates(t *testing.T) {
	p, e, l, events := setup(
		[]ec2types.Instance{inst("i-old", t0), inst("i-new", t0.Add(time.Hour))},
		map[string]elbtypes.TargetHealthStateEnum{"i-old": "healthy", "i-new": "healthy"},
	)

	if err := p.ScaleDown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(l.deregistered) != 1 || l.deregistered[0] != "i-new" {
		t.Errorf("debía quitar la más nueva, quitó %v", l.deregistered)
	}
	if l.drainChecks < 2 {
		t.Error("no esperó al drenaje")
	}
	if len(e.terminated) != 1 || e.terminated[0] != "i-new" {
		t.Errorf("terminadas = %v", e.terminated)
	}
	if got := *events; len(got) != 2 || got[0] != "deregister" || got[1] != "terminate" {
		t.Errorf("orden incorrecto: %v", got)
	}
}

func TestScaleDownPrefersUnhealthy(t *testing.T) {
	p, e, _, _ := setup(
		[]ec2types.Instance{inst("i-bad", t0), inst("i-good", t0.Add(time.Hour))},
		map[string]elbtypes.TargetHealthStateEnum{"i-bad": "unhealthy", "i-good": "healthy"},
	)
	if err := p.ScaleDown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.terminated) != 1 || e.terminated[0] != "i-bad" {
		t.Errorf("debía terminar la no sana, terminó %v", e.terminated)
	}
}

func TestScaleDownRefusesAtMin(t *testing.T) {
	p, e, _, _ := setup([]ec2types.Instance{inst("i-a", t0)}, map[string]elbtypes.TargetHealthStateEnum{"i-a": "healthy"})

	if err := p.ScaleDown(context.Background()); err == nil {
		t.Fatal("esperaba error al bajar del mínimo")
	}
	if len(e.terminated) != 0 {
		t.Error("no debía terminar nada")
	}
}

func TestScaleDownRefusesWhileInstanceWarmingUp(t *testing.T) {
	p, e, l, _ := setup(
		[]ec2types.Instance{inst("i-a", t0), inst("i-b", t0.Add(time.Hour))},
		map[string]elbtypes.TargetHealthStateEnum{"i-a": "healthy", "i-b": "initial"},
	)

	if err := p.ScaleDown(context.Background()); err == nil {
		t.Fatal("esperaba rechazo: hay una instancia inicializando")
	}
	if len(e.terminated) != 0 || len(l.deregistered) != 0 {
		t.Error("no debía tocar nada")
	}
}

func TestScaleDownSkipsDeregisterForUnregistered(t *testing.T) {
	p, e, l, _ := setup(
		[]ec2types.Instance{inst("i-a", t0), inst("i-orphan", t0.Add(time.Hour))},
		map[string]elbtypes.TargetHealthStateEnum{"i-a": "healthy"},
	)
	if err := p.ScaleDown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(l.deregistered) != 0 {
		t.Error("no debía desregistrar una instancia que no está en el target group")
	}
	if len(e.terminated) != 1 || e.terminated[0] != "i-orphan" {
		t.Errorf("terminadas = %v", e.terminated)
	}
}
