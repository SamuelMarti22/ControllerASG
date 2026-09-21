#!/usr/bin/env bash
# Borra todo lo creado por up.sh y termina las instancias gestionadas por el controller.
set -uo pipefail
cd "$(dirname "$0")"
export AWS_REGION="${AWS_REGION:-us-east-1}" AWS_PAGER=""
[ -f state.env ] || { echo "no hay state.env"; exit 1; }
source state.env

IDS=$(aws ec2 describe-instances --filters Name=tag:managed-by,Values=controllerasg Name=instance-state-name,Values=pending,running \
  --query 'Reservations[].Instances[].InstanceId' --output text)
if [ -n "$IDS" ]; then
  echo "== terminando instancias: $IDS"
  aws ec2 terminate-instances --instance-ids $IDS >/dev/null
  aws ec2 wait instance-terminated --instance-ids $IDS
fi

echo "== borrando ALB, target group, launch template y SG"
aws elbv2 delete-load-balancer --load-balancer-arn "$LB_ARN"
aws elbv2 wait load-balancers-deleted --load-balancer-arns "$LB_ARN"
aws elbv2 delete-target-group --target-group-arn "$TG_ARN"
aws ec2 delete-launch-template --launch-template-id "$LT_ID" >/dev/null
# el SG puede tardar en liberarse tras borrar el ALB
for i in 1 2 3 4 5 6; do aws ec2 delete-security-group --group-id "$SG_ID" 2>/dev/null && break; sleep 10; done
rm -f state.env
echo "Listo."
