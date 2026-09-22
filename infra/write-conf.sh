#!/usr/bin/env bash
# Vuelca los IDs de infra/state.env en conf.json (launch template, target group y dimensiones de CloudWatch).
set -euo pipefail
cd "$(dirname "$0")/.."
source infra/state.env
LT_ID="$LT_ID" TG_ARN="$TG_ARN" TG_DIM="$TG_DIM" LB_DIM="$LB_DIM" python3 - <<'PY'
import json, os
d = json.load(open('conf.json'))
d['compute']['launchTemplateId'] = os.environ['LT_ID']
d['loadBalancer']['targetGroupArn'] = os.environ['TG_ARN']
for m in d['metrics']:
    for dim in m.get('dimensions', []):
        dim['value'] = os.environ['TG_DIM'] if dim['name'] == 'TargetGroup' else os.environ['LB_DIM']
json.dump(d, open('conf.json', 'w'), indent=2)
open('conf.json', 'a').write("\n")
PY
echo "conf.json actualizado"
