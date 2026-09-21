#!/usr/bin/env bash
# Crea la infraestructura mínima de prueba: SG, launch template, target group y ALB.
# Guarda los IDs en infra/state.env (lo usa down.sh). Requiere credenciales AWS ya configuradas.
set -euo pipefail
cd "$(dirname "$0")"
export AWS_REGION="${AWS_REGION:-us-east-1}" AWS_PAGER=""

VPC_ID="${VPC_ID:-vpc-00bfffe8086d5270b}"
SUBNETS=(${SUBNETS:-subnet-0e03b12aeb15db9d8 subnet-03b60378a8f640ee3}) # 2 zonas distintas
AMI_ID="${AMI_ID:-ami-0b6d9d3d33ba97d99}"                              # Ubuntu 26.04
KEY_NAME="${KEY_NAME:-PcJamu}"
INSTANCE_TYPE="${INSTANCE_TYPE:-t3.micro}"

[ -f state.env ] && { echo "state.env ya existe: ejecuta ./down.sh primero"; exit 1; }

USER_DATA=$(base64 -w0 <<'UD'
#!/bin/bash
apt-get update -y
apt-get install -y nginx
echo "instancia $(hostname)" > /var/www/html/index.html
systemctl enable --now nginx
UD
)

echo "== security group"
SG_ID=$(aws ec2 create-security-group --group-name asg-web --description "asg-web: HTTP 80" --vpc-id "$VPC_ID" --query GroupId --output text)
aws ec2 authorize-security-group-ingress --group-id "$SG_ID" --protocol tcp --port 80 --cidr 0.0.0.0/0 >/dev/null

echo "== launch template"
LT_ID=$(aws ec2 create-launch-template --launch-template-name asg-web-lt \
  --launch-template-data "{\"ImageId\":\"$AMI_ID\",\"InstanceType\":\"$INSTANCE_TYPE\",\"KeyName\":\"$KEY_NAME\",\"SecurityGroupIds\":[\"$SG_ID\"],\"Monitoring\":{\"Enabled\":true},\"UserData\":\"$USER_DATA\"}" \
  --query LaunchTemplate.LaunchTemplateId --output text)

echo "== target group"
TG_ARN=$(aws elbv2 create-target-group --name asg-web-tg --protocol HTTP --port 80 --vpc-id "$VPC_ID" \
  --health-check-path / --health-check-interval-seconds 10 --healthy-threshold-count 2 --unhealthy-threshold-count 2 \
  --query 'TargetGroups[0].TargetGroupArn' --output text)
aws elbv2 modify-target-group-attributes --target-group-arn "$TG_ARN" --attributes Key=deregistration_delay.timeout_seconds,Value=30 >/dev/null

echo "== load balancer"
LB_ARN=$(aws elbv2 create-load-balancer --name asg-web-alb --subnets "${SUBNETS[@]}" --security-groups "$SG_ID" \
  --query 'LoadBalancers[0].LoadBalancerArn' --output text)
aws elbv2 create-listener --load-balancer-arn "$LB_ARN" --protocol HTTP --port 80 \
  --default-actions Type=forward,TargetGroupArn="$TG_ARN" >/dev/null
LB_DNS=$(aws elbv2 describe-load-balancers --load-balancer-arns "$LB_ARN" --query 'LoadBalancers[0].DNSName' --output text)

cat > state.env <<STATE
SG_ID=$SG_ID
LT_ID=$LT_ID
TG_ARN=$TG_ARN
LB_ARN=$LB_ARN
LB_DNS=$LB_DNS
# dimensiones de CloudWatch (sufijos de los ARN)
TG_DIM=${TG_ARN##*:}
LB_DIM=${LB_ARN##*:loadbalancer/}
STATE
echo "Listo. IDs en infra/state.env"; cat state.env
