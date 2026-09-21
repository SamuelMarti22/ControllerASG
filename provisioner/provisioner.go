package provisioner

import (
	"context"
	"fmt"
	"net"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	elbv2 "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"

	"controllerasg/configuration"
	"controllerasg/model"
)

const (
	// Las instancias que el controller crea llevan este tag; solo esas se cuentan y se tocan.
	ManagedTagKey   = "managed-by"
	ManagedTagValue = "controllerasg"

	healthUnregistered = "unregistered"
	healthHealthy      = "healthy"
)

type EC2API interface {
	RunInstances(ctx context.Context, in *ec2.RunInstancesInput, opts ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, opts ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

type ELBAPI interface {
	RegisterTargets(ctx context.Context, in *elbv2.RegisterTargetsInput, opts ...func(*elbv2.Options)) (*elbv2.RegisterTargetsOutput, error)
	DeregisterTargets(ctx context.Context, in *elbv2.DeregisterTargetsInput, opts ...func(*elbv2.Options)) (*elbv2.DeregisterTargetsOutput, error)
	DescribeTargetHealth(ctx context.Context, in *elbv2.DescribeTargetHealthInput, opts ...func(*elbv2.Options)) (*elbv2.DescribeTargetHealthOutput, error)
}

type Provisioner struct {
	ec2    EC2API
	elb    ELBAPI
	config *configuration.Config

	runningTimeout time.Duration // espera máxima a que una instancia nueva pase a running
	drainTimeout   time.Duration // espera máxima al drenaje de conexiones antes de terminar
	drainPoll      time.Duration
}

func New(ec2Client EC2API, elbClient ELBAPI, cfg *configuration.Config) *Provisioner {
	return &Provisioner{
		ec2:            ec2Client,
		elb:            elbClient,
		config:         cfg,
		runningTimeout: 3 * time.Minute,
		drainTimeout:   2 * time.Minute,
		drainPoll:      5 * time.Second,
	}
}

// Instances devuelve las instancias gestionadas por el controller (pending o
// running) junto con su salud en el target group. Sirve para contar la
// capacidad real y para reconstruir el estado al arrancar.
func (p *Provisioner) Instances(ctx context.Context) ([]model.Instance, error) {
	paginator := ec2.NewDescribeInstancesPaginator(p.ec2, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:" + ManagedTagKey), Values: []string{ManagedTagValue}},
			{Name: aws.String("instance-state-name"), Values: []string{"pending", "running"}},
		},
	})

	var fleet []model.Instance
	for paginator.HasMorePages() {
		out, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("DescribeInstances: %w", err)
		}
		for _, r := range out.Reservations {
			for _, i := range r.Instances {
				inst := model.Instance{Id: aws.ToString(i.InstanceId), SubnetID: aws.ToString(i.SubnetId), Health: healthUnregistered}
				if i.State != nil {
					inst.State = string(i.State.Name)
				}
				if i.LaunchTime != nil {
					inst.LaunchTime = *i.LaunchTime
				}
				if ip := net.ParseIP(aws.ToString(i.PrivateIpAddress)).To4(); ip != nil {
					copy(inst.Ip[:], ip)
				}
				fleet = append(fleet, inst)
			}
		}
	}

	health, err := p.targetHealth(ctx)
	if err != nil {
		return nil, err
	}
	for i := range fleet {
		if h, ok := health[fleet[i].Id]; ok {
			fleet[i].Health = h
		}
	}
	return fleet, nil
}

// ScaleUp lanza una instancia desde el launch template, espera a que esté
// running y la registra en el target group. Si el registro falla, la termina
// para no dejar capacidad huérfana que costaría sin servir tráfico.
func (p *Provisioner) ScaleUp(ctx context.Context) error {
	fleet, err := p.Instances(ctx)
	if err != nil {
		return err
	}
	if len(fleet) >= p.config.Compute.MaxInstances {
		return fmt.Errorf("ya hay %d instancias, el máximo es %d", len(fleet), p.config.Compute.MaxInstances)
	}

	runInput := &ec2.RunInstancesInput{
		SubnetId:         aws.String(pickSubnet(p.config.Compute.SubnetIDs, fleet)),
		SecurityGroupIds: p.config.Compute.SecurityGroupIDs,
		LaunchTemplate: &ec2types.LaunchTemplateSpecification{
			LaunchTemplateId: aws.String(p.config.Compute.LaunchTemplateID),
			Version:          aws.String(p.config.Compute.LaunchTemplateVersion),
		},
		MinCount: aws.Int32(1),
		MaxCount: aws.Int32(1),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeInstance,
			Tags:         []ec2types.Tag{{Key: aws.String(ManagedTagKey), Value: aws.String(ManagedTagValue)}},
		}},
	}
	run, err := p.ec2.RunInstances(ctx, runInput)
	if err != nil {
		return fmt.Errorf("RunInstances: %w", err)
	}
	if len(run.Instances) == 0 {
		return fmt.Errorf("RunInstances no devolvió instancias")
	}
	id := aws.ToString(run.Instances[0].InstanceId)

	waiter := ec2.NewInstanceRunningWaiter(p.ec2)
	if err := waiter.Wait(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}}, p.runningTimeout); err != nil {
		p.rollback(id)
		return fmt.Errorf("la instancia %s no llegó a running: %w", id, err)
	}

	_, err = p.elb.RegisterTargets(ctx, &elbv2.RegisterTargetsInput{
		TargetGroupArn: aws.String(p.config.LoadBalancer.TargetGroupARN),
		Targets:        []elbtypes.TargetDescription{{Id: aws.String(id), Port: aws.Int32(int32(p.config.LoadBalancer.TargetPort))}},
	})
	if err != nil {
		p.rollback(id)
		return fmt.Errorf("RegisterTargets de %s: %w", id, err)
	}
	return nil
}

// ScaleDown elige una instancia, la saca del target group, espera a que drene
// sus conexiones y la termina.
func (p *Provisioner) ScaleDown(ctx context.Context) error {
	fleet, err := p.Instances(ctx)
	if err != nil {
		return err
	}
	if len(fleet) <= p.config.Compute.MinInstances {
		return fmt.Errorf("hay %d instancias, el mínimo es %d", len(fleet), p.config.Compute.MinInstances)
	}

	if warming := warmingUp(fleet); warming != "" {
		return fmt.Errorf("no es seguro reducir: %s aún se está inicializando", warming)
	}

	victim := pickVictim(fleet)

	if victim.Health != healthUnregistered {
		_, err := p.elb.DeregisterTargets(ctx, &elbv2.DeregisterTargetsInput{
			TargetGroupArn: aws.String(p.config.LoadBalancer.TargetGroupARN),
			Targets:        []elbtypes.TargetDescription{{Id: aws.String(victim.Id), Port: aws.Int32(int32(p.config.LoadBalancer.TargetPort))}},
		})
		if err != nil {
			return fmt.Errorf("DeregisterTargets de %s: %w", victim.Id, err)
		}
		p.waitDrained(ctx, victim.Id)
	}

	if _, err := p.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{victim.Id}}); err != nil {
		return fmt.Errorf("TerminateInstances de %s: %w", victim.Id, err)
	}
	return nil
}

// pickSubnet reparte la flota entre zonas de disponibilidad: elige la subred
// con menos instancias gestionadas (a igualdad, la primera del config).
func pickSubnet(subnets []string, fleet []model.Instance) string {
	count := make(map[string]int, len(subnets))
	for _, i := range fleet {
		count[i.SubnetID]++
	}
	best := subnets[0]
	for _, s := range subnets[1:] {
		if count[s] < count[best] {
			best = s
		}
	}
	return best
}

// pickVictim prefiere las instancias que menos aportan (no sanas) y, entre
// iguales, la más nueva: la más antigua ya está caliente y sirviendo.
func pickVictim(fleet []model.Instance) model.Instance {
	sorted := append([]model.Instance(nil), fleet...)
	sort.SliceStable(sorted, func(a, b int) bool {
		ha, hb := sorted[a].Health == healthHealthy, sorted[b].Health == healthHealthy
		if ha != hb {
			return !ha
		}
		return sorted[a].LaunchTime.After(sorted[b].LaunchTime)
	})
	return sorted[0]
}

// warmingUp devuelve el id de una instancia que todavía no está lista para
// servir (pending o initial en el target group), o "" si no hay ninguna.
// Reducir mientras la capacidad nueva aún no aporta sería contradictorio.
func warmingUp(fleet []model.Instance) string {
	for _, i := range fleet {
		if i.State == string(ec2types.InstanceStateNamePending) || i.Health == string(elbtypes.TargetHealthStateEnumInitial) {
			return i.Id
		}
	}
	return ""
}

// waitDrained espera a que el target deje de estar en "draining". Si se agota
// el tiempo continúa: es preferible terminar a quedarse bloqueado.
func (p *Provisioner) waitDrained(ctx context.Context, id string) {
	deadline := time.Now().Add(p.drainTimeout)
	for time.Now().Before(deadline) {
		health, err := p.targetHealth(ctx)
		if err != nil || health[id] != string(elbtypes.TargetHealthStateEnumDraining) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.drainPoll):
		}
	}
}

func (p *Provisioner) targetHealth(ctx context.Context) (map[string]string, error) {
	out, err := p.elb.DescribeTargetHealth(ctx, &elbv2.DescribeTargetHealthInput{
		TargetGroupArn: aws.String(p.config.LoadBalancer.TargetGroupARN),
	})
	if err != nil {
		return nil, fmt.Errorf("DescribeTargetHealth: %w", err)
	}
	health := make(map[string]string, len(out.TargetHealthDescriptions))
	for _, d := range out.TargetHealthDescriptions {
		if d.Target != nil && d.TargetHealth != nil {
			health[aws.ToString(d.Target.Id)] = string(d.TargetHealth.State)
		}
	}
	return health, nil
}

// rollback termina una instancia que no se pudo dejar operativa. Usa su propio
// contexto porque el original puede estar cancelado.
func (p *Provisioner) rollback(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}})
}
