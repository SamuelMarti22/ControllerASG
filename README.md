# ControllerASG

Un **controlador de auto-scaling** escrito en Go: una reimplementación didáctica
y autogestionada de lo que hace un *AWS Auto Scaling Group*, pero controlada por
ti en lugar de por AWS.

En vez de crear un ASG nativo, el programa gestiona directamente instancias EC2
individuales (etiquetadas con `managed-by=controllerasg`): consulta métricas en
CloudWatch, decide si hace falta más o menos capacidad y actúa sobre EC2 y el
Target Group de un Application Load Balancer. Cada decisión queda registrada en
un log auditable.

---

## Índice

- [Resumen](#resumen)
- [Funcionalidades](#funcionalidades)
- [Arquitectura](#arquitectura)
- [Flujo de ejecución](#flujo-de-ejecución)
- [Lógica de decisión](#lógica-de-decisión)
- [Tecnologías](#tecnologías)
- [Requisitos previos](#requisitos-previos)
- [Instalación](#instalación)
  - [1. Instalar herramientas](#1-instalar-herramientas)
  - [2. Credenciales y permisos de AWS](#2-credenciales-y-permisos-de-aws)
  - [3. Provisionar la infraestructura de prueba (opcional)](#3-provisionar-la-infraestructura-de-prueba-opcional)
  - [4. Configurar y arrancar con init.sh](#4-configurar-y-arrancar-con-initsh)
- [Configuración (conf.json)](#configuración-confjson)
- [Uso](#uso)
- [El log de decisiones](#el-log-de-decisiones)
- [Ejecutar en una instancia EC2](#ejecutar-en-una-instancia-ec2)
- [Tests](#tests)
- [Limitaciones y notas](#limitaciones-y-notas)
- [Estructura del proyecto](#estructura-del-proyecto)

---

## Resumen

ControllerASG implementa un **bucle de control** (observar → decidir → actuar)
que se ejecuta periódicamente:

1. **Observa** el estado real de la flota (instancias EC2 gestionadas y su salud
   en el Target Group) y sus métricas en CloudWatch.
2. **Decide** si aumentar, reducir o mantener la capacidad, según umbrales
   configurables y reglas de seguridad (mínimos, cooldown, calentamiento).
3. **Actúa** lanzando o retirando una instancia, registrándola/desregistrándola
   del Target Group, con drenaje de conexiones y *rollback* ante fallos.

Todo lo que ve, decide y ejecuta se anota como una línea JSON en un log, de modo
que siempre puedes reconstruir *qué pasó y por qué*.

---

## Funcionalidades

- **Escalado horizontal automático** de instancias EC2 entre un mínimo y un
  máximo configurables (1 a 5 por defecto).
- **Múltiples métricas simultáneas** de CloudWatch, combinables:
  - `RequestCountPerTarget` — demanda (requests por instancia).
  - `TargetResponseTime` — calidad de servicio (latencia).
  - `CPUUtilization` — uso de recurso (por instancia, se promedia la flota).
- **Escalado asimétrico**: sube si *cualquier* métrica supera su umbral (reacción
  rápida); baja solo si *todas* están por debajo (conservador).
- **Reglas de seguridad**:
  - Piso de capacidad garantizado (restablece el mínimo aunque no haya métricas).
  - *Cooldown* entre acciones para evitar oscilaciones.
  - No reduce mientras haya instancias todavía inicializándose.
- **Operaciones seguras sobre la infraestructura**:
  - Registro en el Target Group y espera a estado `running` al escalar hacia arriba.
  - *Rollback* automático (termina la instancia) si no llega a operativa.
  - Drenaje de conexiones antes de terminar al reducir.
  - Reparto de instancias entre zonas de disponibilidad (subredes).
- **Auditoría completa**: un `DecisionRecord` en formato JSON Lines por cada ciclo.
- **Validación de configuración** independiente (`-check`) sin contactar AWS.
- **Apagado limpio** ante `SIGINT`/`SIGTERM` (compatible con systemd y Ctrl+C).
- **Script interactivo** (`init.sh`) para generar la configuración, compilar,
  validar y arrancar.

---

## Arquitectura

El diseño separa responsabilidades en módulos, con **interfaces** que aíslan la
lógica de decisión de las llamadas a AWS (lo que la hace fácilmente testeable):

```
                         ┌───────────────────────────────────────────┐
                         │                  main                      │
                         │  arma dependencias, ticker, señales SO     │
                         └───────────────────────┬───────────────────┘
                                                 │
                              ┌──────────────────▼──────────────────┐
                              │             controller              │
                              │  Tick(): observar → decidir → actuar│
                              │  y registra 1 DecisionRecord        │
                              └──┬───────────┬───────────┬──────────┘
                                 │           │           │
              MetricsSource ─────┘           │           └───── Actuator / Fleet
                    │                        │                        │
            ┌───────▼────────┐        ┌──────▼──────┐         ┌───────▼────────┐
            │    observer    │        │    logger   │         │   provisioner  │
            │  CloudWatch    │        │  JSON Lines │         │  EC2 + ELBv2   │
            │  GetMetricData │        └─────────────┘         │  run/terminate │
            └───────┬────────┘                                │  register/     │
                    │                                         │  deregister    │
              ┌─────▼─────┐                                   └───────┬────────┘
              │ CloudWatch│                                      ┌────▼────┐
              └───────────┘                                      │ EC2/ALB │
                                                                 └─────────┘
```

Módulos:

| Paquete         | Responsabilidad |
|-----------------|-----------------|
| `main`          | Punto de entrada: parsea flags, arma dependencias, ejecuta el ticker y gestiona señales del sistema. |
| `configuration` | Carga y **valida** `conf.json`. |
| `controller`    | Orquesta el ciclo y contiene la **lógica de decisión**. Depende solo de interfaces (`MetricsSource`, `Fleet`, `Actuator`, `Logger`). |
| `observer`      | Traduce las políticas de métrica a consultas `GetMetricData` de CloudWatch, con paginación y agregación por ventana. |
| `provisioner`   | Implementa `Fleet` y `Actuator`: lanza/termina instancias EC2 y las registra/desregistra del Target Group (ELBv2). |
| `logger`        | Escribe un `DecisionRecord` por línea en JSON Lines (thread-safe). |
| `model`         | Tipos de dominio: `Instance`, `ObservedMetric`, `Decision`, `DecisionRecord`, `Capacity`. |
| `infra`         | Scripts para crear/destruir la infraestructura de prueba (SG, launch template, target group, ALB). |

---

## Flujo de ejecución

En cada tick (cada `pollIntervalSeconds`), `controller.Tick()`:

1. **Lee la flota** con `provisioner.Instances()`: instancias `pending`/`running`
   con el tag `managed-by=controllerasg` y su salud en el Target Group. Si falla,
   no actúa (sin saber la capacidad no es seguro decidir).
2. **Observa las métricas** con `observer.Observe()`. Las métricas sin datapoints
   se omiten (no se reportan como `0`).
3. **Decide** (`INCREASE_CAPACITY`, `REDUCE_CAPACITY` o `MAINTAIN_CAPACITY`).
4. **Actúa** si corresponde:
   - *ScaleUp*: `RunInstances` desde el launch template → espera `running` →
     `RegisterTargets`. Si algo falla, hace *rollback* (termina la instancia).
   - *ScaleDown*: elige la víctima → `DeregisterTargets` → espera drenaje →
     `TerminateInstances`.
5. **Registra** exactamente un `DecisionRecord`, incluso ante errores.

---

## Lógica de decisión

El orden de las reglas (en `controller.decide()`) es deliberado:

1. **Piso de seguridad** — si `capacidad < minInstances`, sube siempre,
   ignorando métricas y cooldown (cubre arranque en frío o una instancia caída).
2. **Cooldown** — si la última acción fue hace menos de `cooldownSeconds`, mantiene.
3. **Sin métricas** — si no llegó ningún dato, mantiene (no se actúa a ciegas).
4. **Umbrales**:
   - **Sube** si *alguna* métrica ≥ su `scaleUpThreshold` (y hay margen bajo el máximo).
   - **Reduce** si *todas* las métricas < su `scaleDownThreshold` (y hay margen sobre el mínimo).
   - En cualquier otro caso, mantiene.

Una métrica sin dato **se abstiene**: no fuerza subida ni bloquea la reducción.
Para métricas de demanda, la ausencia de datapoints suele significar "no hubo
tráfico", que es justamente señal de que es seguro reducir.

---

## Tecnologías

- **Go 1.27** (ver `go.mod`).
- **AWS SDK for Go v2**:
  - `service/ec2` — lanzar, terminar y describir instancias.
  - `service/elasticloadbalancingv2` — registrar/desregistrar targets y consultar su salud.
  - `service/cloudwatch` — `GetMetricData` para observar métricas.
- **Bash** + **Python 3** — scripts de configuración (`init.sh`) e infraestructura (`infra/`).
- **AWS CLI** — solo para los scripts de `infra/` (crear/destruir recursos de prueba).

---

## Requisitos previos

- **Go 1.27+**
- **python3** (lo usa `init.sh` para leer/escribir `conf.json`)
- **Credenciales de AWS** configuradas con permisos suficientes (ver abajo)
- **AWS CLI** — solo si vas a usar los scripts de `infra/`
- Recursos de AWS ya existentes (o creados con `infra/up.sh`):
  - un **Launch Template**,
  - un **Target Group** de un **Application Load Balancer**,
  - al menos una **subred** (idealmente dos, en zonas distintas).

---

## Instalación

### 1. Instalar herramientas

**Go** (Ubuntu/Debian; se recomienda el tarball oficial para tener 1.27+):

```bash
# Opción rápida (puede traer una versión más antigua):
sudo apt-get update && sudo apt-get install -y golang-go python3

# Opción recomendada (versión concreta):
curl -LO https://go.dev/dl/go1.27.1.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.27.1.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin   # añádelo a ~/.bashrc para que persista
go version
```

**AWS CLI** (solo si usarás `infra/`):

```bash
sudo apt-get install -y awscli   # o la instalación oficial de AWS CLI v2
```

Clonar el repositorio:

```bash
git clone <URL-del-repo> ControllerASG
cd ControllerASG
```

### 2. Credenciales y permisos de AWS

En tu máquina local, con `aws configure` (perfil por defecto) o variables de
entorno. En una instancia EC2, lo ideal es un **IAM Instance Role** (ver
[Ejecutar en una instancia EC2](#ejecutar-en-una-instancia-ec2)).

Permisos mínimos que necesita el controller:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:RunInstances",
        "ec2:TerminateInstances",
        "ec2:DescribeInstances",
        "ec2:CreateTags",
        "elasticloadbalancing:RegisterTargets",
        "elasticloadbalancing:DeregisterTargets",
        "elasticloadbalancing:DescribeTargetHealth",
        "cloudwatch:GetMetricData"
      ],
      "Resource": "*"
    }
  ]
}
```

> Los scripts de `infra/` requieren además permisos para crear/borrar SG,
> launch templates, target groups y load balancers.

### 3. Provisionar la infraestructura de prueba (opcional)

Si aún no tienes launch template + ALB + target group, puedes crearlos con el
script incluido (levanta un SG con HTTP:80, un launch template con nginx vía
user-data, un target group y un ALB):

```bash
cd infra
./up.sh          # crea todo y guarda los IDs en infra/state.env
```

Puedes personalizar `VPC_ID`, `SUBNETS`, `AMI_ID`, `KEY_NAME`, `INSTANCE_TYPE`
mediante variables de entorno antes de ejecutarlo.

Para volcar esos IDs a un `conf.json` ya existente:

```bash
./write-conf.sh  # copia launch template, target group y dimensiones a conf.json
```

Para destruir todo lo creado (y terminar las instancias gestionadas):

```bash
./down.sh
```

### 4. Configurar y arrancar con init.sh

`init.sh` es la vía recomendada para el primer arranque. De forma interactiva:

- Pregunta cada valor de `conf.json` (si ya existe, ofrece los actuales como
  default; **Enter** los conserva).
- Deriva automáticamente las dimensiones de CloudWatch a partir de los ARN del
  Target Group y del Load Balancer.
- Permite activar/desactivar cada métrica y fijar sus umbrales.
- Compila el binario en `bin/controllerasg`.
- Valida la configuración con `-check` (sin contactar AWS).
- Opcionalmente arranca el controller.

```bash
./init.sh
```

---

## Configuración (conf.json)

El archivo `conf.json` describe toda la operación. Ejemplo:

```json
{
  "aws": { "region": "us-east-1" },
  "compute": {
    "launchTemplateId": "lt-0123456789abcdef0",
    "launchTemplateVersion": "$Latest",
    "minInstances": 1,
    "maxInstances": 5,
    "subnetIds": ["subnet-aaaa", "subnet-bbbb"],
    "securityGroupIds": []
  },
  "loadBalancer": {
    "targetGroupArn": "arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/mi-tg/xxxx",
    "targetPort": 80
  },
  "pollIntervalSeconds": 60,
  "observationWindowSeconds": 300,
  "cooldownSeconds": 300,
  "metrics": [
    {
      "namespace": "AWS/ApplicationELB",
      "metricName": "RequestCountPerTarget",
      "dimensions": [
        { "name": "TargetGroup",  "value": "targetgroup/mi-tg/xxxx" },
        { "name": "LoadBalancer", "value": "app/mi-alb/yyyy" }
      ],
      "period": 60,
      "stat": "Sum",
      "scaling": { "scaleUpThreshold": 1000, "scaleDownThreshold": 200 }
    },
    {
      "namespace": "AWS/EC2",
      "metricName": "CPUUtilization",
      "perInstance": true,
      "period": 60,
      "stat": "Average",
      "scaling": { "scaleUpThreshold": 70, "scaleDownThreshold": 20 }
    }
  ],
  "deployment": { "useDocker": false }
}
```

Campos principales:

| Campo | Descripción |
|-------|-------------|
| `aws.region` | Región de AWS. **Obligatorio.** |
| `compute.launchTemplateId` | Launch template para crear instancias. **Obligatorio.** |
| `compute.launchTemplateVersion` | Versión (`$Latest`, `$Default` o un número). |
| `compute.minInstances` / `maxInstances` | Límites de capacidad. `min ≥ 1`, `max ≤ 5`, `min ≤ max`. |
| `compute.subnetIds` | Subredes donde repartir instancias (al menos 1). |
| `compute.securityGroupIds` | SGs; vacío usa los del launch template. |
| `loadBalancer.targetGroupArn` | Target Group donde registrar instancias. **Obligatorio.** |
| `loadBalancer.targetPort` | Puerto de la aplicación en las instancias. |
| `pollIntervalSeconds` | Segundos entre ciclos. `> 0`. |
| `observationWindowSeconds` | Ventana de observación; `≥ period` de cada métrica. |
| `cooldownSeconds` | Espera mínima entre acciones. `≥ 0`. |
| `metrics[]` | Políticas de métrica (al menos 1). |
| `metrics[].perInstance` | Si `true`, se consulta por instancia (dimensión `InstanceId`) y **no** debe traer `dimensions`. |
| `metrics[].period` | Múltiplo positivo de 60. |
| `metrics[].scaling` | `scaleUpThreshold` debe ser **mayor** que `scaleDownThreshold`. |
| `deployment.useDocker` | Marca informativa de despliegue. |

Reglas de validación (aplicadas por `-check` y al arrancar): región, launch
template y target group obligatorios; `1 ≤ min ≤ max ≤ 5`; `pollIntervalSeconds > 0`;
`cooldownSeconds ≥ 0`; al menos una métrica; `period` múltiplo de 60;
`observationWindowSeconds ≥ period`; `scaleUp > scaleDown`; `perInstance` sin dimensiones.

---

## Uso

Compilar manualmente:

```bash
mkdir -p bin
go build -o bin/controllerasg ./main
```

Flags disponibles:

| Flag | Default | Descripción |
|------|---------|-------------|
| `-config` | `conf.json` | Ruta del archivo de configuración. |
| `-log` | `decisions.jsonl` | Archivo donde se anota cada decisión (JSON Lines). |
| `-check` | `false` | Solo carga y valida la configuración; no contacta AWS ni inicia el ciclo. |

Validar la configuración sin tocar AWS:

```bash
./bin/controllerasg -config conf.json -check
```

Arrancar el controller (Ctrl+C para detener):

```bash
./bin/controllerasg -config conf.json -log decisions.jsonl
```

---

## El log de decisiones

Cada ciclo produce una línea JSON en el archivo de log. Ejemplo:

```json
{
  "Time": "2026-09-22T16:01:48-05:00",
  "Decision": "REDUCE_CAPACITY",
  "Reason": "todas las métricas están por debajo de su umbral de bajada",
  "WindowSeconds": 300,
  "Metrics": [
    { "Name": "RequestCountPerTarget", "Value": 0, "timestamp": "2026-09-22T16:00:00-05:00" },
    { "Name": "CPUUtilization", "Value": 0.12, "timestamp": "2026-09-22T16:00:00-05:00" }
  ],
  "Capacity": {
    "Total": 2, "Healthy": 2,
    "Instances": [
      { "ID": "i-0aad...", "State": "running", "Health": "healthy" },
      { "ID": "i-0866...", "State": "running", "Health": "healthy" }
    ]
  },
  "InstanceID": "i-0866...",
  "Action": "ScaleDown",
  "Result": "ok",
  "Error": ""
}
```

Permite reconstruir qué se observó, qué se decidió y por qué, qué se pidió a la
infraestructura y qué resultó. Al ser JSON Lines, se puede procesar con `jq`:

```bash
tail -f decisions.jsonl | jq '{t: .Time, d: .Decision, r: .Reason}'
```

---

## Ejecutar en una instancia EC2

El binario es portable, pero el entorno no viaja con el repo. Para correrlo en
una EC2:

1. **Go y python3** instalados en la instancia (no vienen por defecto).
2. **IAM Instance Role** con los permisos de arriba (no copies claves; el SDK
   detecta el rol automáticamente vía metadata).
3. **`conf.json`** apuntando a recursos que existan en esa cuenta/región.
4. **La EC2 del controller debe estar FUERA de la flota gestionada** (sin el tag
   `managed-by=controllerasg`), o el controller podría terminarse a sí mismo en
   un *ScaleDown*.
5. **Un solo controller a la vez** por conjunto de recursos: dos instancias
   apuntando al mismo Target Group y tag tomarían decisiones contradictorias.

Para que sobreviva a cierres de sesión y reinicios, ejecútalo bajo **systemd**
(el código ya maneja `SIGTERM` limpiamente). Ejemplo de unit:

```ini
# /etc/systemd/system/controllerasg.service
[Unit]
Description=ControllerASG
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/ControllerASG
ExecStart=/opt/ControllerASG/bin/controllerasg -config /opt/ControllerASG/conf.json -log /opt/ControllerASG/decisions.jsonl
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now controllerasg
journalctl -u controllerasg -f
```

---

## Tests

```bash
go test ./...              # todos los tests
go test -count=1 -cover ./...   # sin caché, con cobertura
go vet ./...               # análisis estático
```

Los módulos críticos están cubiertos con tests unitarios (usando *mocks* de las
APIs de AWS mediante las interfaces del proyecto) y hay tests de integración
adicionales en `observer/` y `provisioner/`.

---

## Limitaciones y notas

- **El cooldown vive solo en memoria.** Tras un reinicio del proceso, el
  controller olvida cuándo fue la última acción y puede actuar en el primer tick.
- **Sin coordinación entre instancias.** No hay *locking* ni elección de líder:
  ejecuta un único controller por conjunto de recursos.
- **La actuación bloquea el ciclo.** Un *ScaleUp* espera hasta 3 min a que la
  instancia esté `running`; durante ese tiempo no se procesan nuevos ticks.
- **Alcance de prueba/educativo**: pensado para flotas pequeñas (máx. 5
  instancias) y un objetivo de aprendizaje, no como reemplazo de un ASG en producción.

---

## Estructura del proyecto

```
ControllerASG/
├── main/            # punto de entrada
├── controller/     # bucle observar→decidir→actuar y lógica de decisión
├── observer/       # consulta de métricas en CloudWatch
├── provisioner/    # actuación sobre EC2 y ELBv2 (Fleet + Actuator)
├── configuration/  # carga y validación de conf.json
├── logger/         # log de decisiones en JSON Lines
├── model/          # tipos de dominio
├── infra/          # scripts para crear/destruir infraestructura de prueba
├── init.sh         # configurar + compilar + validar + arrancar (interactivo)
├── conf.json       # configuración activa (no versionada)
├── conf.example.json
└── decisions.jsonl # log de decisiones (generado)
```
