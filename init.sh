#!/usr/bin/env bash
# init.sh: pregunta los datos de conf.json, valida la configuración y arranca
# el controller. Pensado para el primer arranque en una máquina nueva.
#
#   ./init.sh
#
# Si ya existe conf.json en el directorio, sus valores se ofrecen como
# default (Enter los conserva). Requiere Go y credenciales de AWS ya
# configuradas (ver README.md); este script no toca la cuenta de AWS, solo
# escribe conf.json y valida su forma.
set -euo pipefail
cd "$(dirname "$0")"

CONF="conf.json"
LOG="decisions.jsonl"

command -v go >/dev/null || { echo "Falta Go. Instálalo antes de continuar."; exit 1; }
command -v python3 >/dev/null || { echo "Falta python3 (se usa para leer/escribir conf.json)."; exit 1; }

# ---- helpers ---------------------------------------------------------

# current <path.con.puntos[e.idx]> -> valor actual de conf.json, o "" si no existe el archivo/campo.
# Admite índices de lista, p. ej. "metrics[0].dimensions[1].value".
current() {
	[ -f "$CONF" ] || return 0
	python3 - "$1" <<'PY' 2>/dev/null || true
import json, re, sys
if not sys.argv[1]:
	sys.exit(0)  # sin ruta, no hay nada que buscar (evita volcar el JSON completo)
tokens = re.findall(r'[^.\[\]]+|\[\d+\]', sys.argv[1])
try:
	d = json.load(open("conf.json"))
	for t in tokens:
		d = d[int(t[1:-1])] if t.startswith("[") else d[t]
	print(d if not isinstance(d, list) else ",".join(d))
except Exception:
	pass
PY
}

# ask <prompt> <path.en.puntos> [default-si-no-hay-actual] -> lee stdin, imprime el valor elegido
ask() {
	local prompt="$1" default
	default="$(current "$2")"
	[ -n "$default" ] || default="${3:-}"
	local reply
	if [ -n "$default" ]; then
		read -r -p "$prompt [$default]: " reply
	else
		read -r -p "$prompt: " reply
	fi
	echo "${reply:-$default}"
}

ask_yn() {
	local prompt="$1" default="${2:-n}" reply label="s/N"
	[ "$default" = "s" ] && label="S/n"
	read -r -p "$prompt [$label]: " reply
	reply="${reply:-$default}"
	case "$reply" in [sSyY]*) echo true ;; *) echo false ;; esac
}

# metric_field <metricName> <campo.con.puntos> -> valor actual de esa métrica
# en conf.json (buscada por metricName, no por posición), o "" si no existe.
metric_field() {
	[ -f "$CONF" ] || return 0
	python3 - "$1" "$2" <<'PY' 2>/dev/null || true
import json, sys
name, field = sys.argv[1], sys.argv[2]
try:
	d = json.load(open("conf.json"))
	for m in d.get("metrics", []):
		if m.get("metricName") == name:
			v = m
			for part in field.split("."):
				v = v[part]
			print(v)
			break
except Exception:
	pass
PY
}

# ask_metric <metricName> <etiqueta> <up-default> <down-default>
# Pregunta si esa métrica se usa; si sí, pide sus umbrales. Dejan el resultado
# en $METRIC_ENABLED/$METRIC_UP/$METRIC_DOWN. Por default se activa la métrica
# si ya estaba en el conf.json existente (o si es la primera vez que se corre).
ask_metric() {
	local name="$1" label="$2" default_up="$3" default_down="$4" default_enabled="s"
	if [ -f "$CONF" ]; then
		[ -n "$(metric_field "$name" metricName)" ] || default_enabled="n"
	fi
	METRIC_ENABLED=$(ask_yn "¿Usar $label ($name)?" "$default_enabled")
	if [ "$METRIC_ENABLED" = "true" ]; then
		local cur_up cur_down
		cur_up="$(metric_field "$name" scaling.scaleUpThreshold)"
		cur_down="$(metric_field "$name" scaling.scaleDownThreshold)"
		METRIC_UP=$(ask "    subir capacidad por encima de" "" "${cur_up:-$default_up}")
		METRIC_DOWN=$(ask "    reducir capacidad por debajo de" "" "${cur_down:-$default_down}")
	fi
}

echo "== Configuración del Auto-Scaling Controller =="
echo "Enter conserva el valor entre corchetes (tomado de $CONF si ya existe)."
echo

region=$(ask "Región de AWS" "aws.region" "us-east-1")
lt_id=$(ask "Launch Template ID (lt-...)" "compute.launchTemplateId")
lt_version=$(ask "Versión del Launch Template" "compute.launchTemplateVersion" '$Latest')
min_instances=$(ask "Mínimo de instancias (>=1)" "compute.minInstances" "1")
max_instances=$(ask "Máximo de instancias (<=5)" "compute.maxInstances" "5")
subnet_ids=$(ask "Subredes, separadas por coma (al menos 1, en zonas distintas)" "compute.subnetIds")
sg_ids=$(ask "Security groups, separados por coma (vacío = los del launch template)" "compute.securityGroupIds" "")

tg_arn=$(ask "ARN del Target Group" "loadBalancer.targetGroupArn")
target_port=$(ask "Puerto de la aplicación en las instancias" "loadBalancer.targetPort" "80")

# Las dimensiones de CloudWatch se derivan del ARN del target group y del load
# balancer para no pedirle al usuario que las escriba a mano (son el sufijo
# tras la última ':').
tg_dim="${tg_arn##*:}"
read -r -p "ARN del Load Balancer (solo para derivar la dimensión de CloudWatch; Enter para conservar la actual): " lb_arn
if [ -z "$lb_arn" ]; then
	lb_dim=$(current "metrics[0].dimensions[1].value" 2>/dev/null || true)
	lb_dim="${lb_dim:-}"
else
	lb_dim="${lb_arn##*:loadbalancer/}"
fi
if [ -z "$lb_dim" ]; then
	echo "No se pudo derivar la dimensión del Load Balancer; escríbela tal cual (p. ej. app/mi-alb/xxxx):"
	read -r -p "Dimensión LoadBalancer: " lb_dim
fi

poll_interval=$(ask "Segundos entre ciclos de observación" "pollIntervalSeconds" "60")
window=$(ask "Ventana de observación en segundos (>= period de las métricas)" "observationWindowSeconds" "300")
cooldown=$(ask "Cooldown entre acciones, en segundos" "cooldownSeconds" "300")

echo
echo "-- Métricas a considerar (responde 'n' para dejar una por fuera del todo)"

echo "  · RequestCountPerTarget — demanda: requests entrantes por instancia (suma/periodo)"
ask_metric "RequestCountPerTarget" "requests entrantes" "1000" "200"
req_enabled="$METRIC_ENABLED"; req_up="${METRIC_UP:-}"; req_down="${METRIC_DOWN:-}"

echo "  · TargetResponseTime — calidad de servicio: latencia de respuesta (segundos)"
ask_metric "TargetResponseTime" "latencia de respuesta" "0.5" "0.1"
lat_enabled="$METRIC_ENABLED"; lat_up="${METRIC_UP:-}"; lat_down="${METRIC_DOWN:-}"

echo "  · CPUUtilization — recurso: uso de CPU promedio de la flota (%)"
ask_metric "CPUUtilization" "uso de CPU" "70" "20"
cpu_enabled="$METRIC_ENABLED"; cpu_up="${METRIC_UP:-}"; cpu_down="${METRIC_DOWN:-}"

if [ "$req_enabled" != "true" ] && [ "$lat_enabled" != "true" ] && [ "$cpu_enabled" != "true" ]; then
	echo "Debes dejar activa al menos una métrica; no se puede escalar sin ninguna señal." >&2
	exit 1
fi

use_docker=$(ask_yn "¿Desplegado con Docker?" "n")
use_docker_py="False"; [ "$use_docker" = "true" ] && use_docker_py="True"

python3 <<PY
import json
metrics = []
if "$req_enabled" == "true":
	metrics.append({
		"namespace": "AWS/ApplicationELB", "metricName": "RequestCountPerTarget",
		"dimensions": [{"name": "TargetGroup", "value": "$tg_dim"}, {"name": "LoadBalancer", "value": "$lb_dim"}],
		"period": 60, "stat": "Sum",
		"scaling": {"scaleUpThreshold": float("$req_up"), "scaleDownThreshold": float("$req_down")},
	})
if "$lat_enabled" == "true":
	metrics.append({
		"namespace": "AWS/ApplicationELB", "metricName": "TargetResponseTime",
		"dimensions": [{"name": "TargetGroup", "value": "$tg_dim"}, {"name": "LoadBalancer", "value": "$lb_dim"}],
		"period": 60, "stat": "Average",
		"scaling": {"scaleUpThreshold": float("$lat_up"), "scaleDownThreshold": float("$lat_down")},
	})
if "$cpu_enabled" == "true":
	metrics.append({
		"namespace": "AWS/EC2", "metricName": "CPUUtilization", "perInstance": True,
		"period": 60, "stat": "Average",
		"scaling": {"scaleUpThreshold": float("$cpu_up"), "scaleDownThreshold": float("$cpu_down")},
	})

d = {
	"aws": {"region": "$region"},
	"compute": {
		"launchTemplateId": "$lt_id",
		"launchTemplateVersion": "$lt_version",
		"minInstances": int("$min_instances"),
		"maxInstances": int("$max_instances"),
		"subnetIds": [s.strip() for s in "$subnet_ids".split(",") if s.strip()],
		"securityGroupIds": [s.strip() for s in "$sg_ids".split(",") if s.strip()],
	},
	"loadBalancer": {"targetGroupArn": "$tg_arn", "targetPort": int("$target_port")},
	"pollIntervalSeconds": int("$poll_interval"),
	"observationWindowSeconds": int("$window"),
	"cooldownSeconds": int("$cooldown"),
	"metrics": metrics,
	"deployment": {"useDocker": $use_docker_py},
}
json.dump(d, open("$CONF", "w"), indent=2)
open("$CONF", "a").write("\n")
PY
echo
echo "== conf.json escrito =="

echo "== Compilando =="
mkdir -p bin
go build -o bin/controllerasg ./main

echo "== Validando configuración (sin contactar AWS) =="
./bin/controllerasg -config "$CONF" -check

echo
if [ "$(ask_yn "¿Iniciar el controller ahora?" "s")" = "true" ]; then
	echo "== Iniciando (Ctrl+C para detener) =="
	exec ./bin/controllerasg -config "$CONF" -log "$LOG"
else
	echo "Cuando quieras arrancarlo: ./bin/controllerasg -config $CONF -log $LOG"
fi
