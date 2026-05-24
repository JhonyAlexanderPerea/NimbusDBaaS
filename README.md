# NimbusDBaaS

**Prototipo funcional de un servicio administrado de bases de datos (DBaaS)**  
*Computación en la nube 2026-1 — Nota Parcial #3*

---

## Tabla de contenido

1. [Descripción general](#descripción-general)
2. [Arquitectura del sistema](#arquitectura-del-sistema)
3. [Estructura del proyecto](#estructura-del-proyecto)
4. [Requisitos previos](#requisitos-previos)
5. [Instalación y ejecución](#instalación-y-ejecución)
6. [Modo simulado vs. modo real](#modo-simulado-vs-modo-real)
7. [Configuración de plantillas VirtualBox (modo real)](#configuración-de-plantillas-virtualbox-modo-real)
8. [Variables de entorno](#variables-de-entorno)
9. [API REST](#api-rest)
10. [Flujo de aprovisionamiento](#flujo-de-aprovisionamiento)
11. [Interfaz de usuario](#interfaz-de-usuario)

---

## Descripción general

NimbusDBaaS es un prototipo de *Database as a Service* que simula el comportamiento de servicios como Amazon RDS o Google Cloud SQL, pero ejecutado completamente en un entorno local con Oracle VirtualBox.

El sistema permite a los usuarios:

- Crear instancias de bases de datos MariaDB o PostgreSQL con un solo formulario web.
- Especificar el nombre de la base de datos, el usuario administrador y (opcionalmente) un archivo `.sql` de inicialización.
- Ver las instancias activas con sus credenciales de acceso (host, puerto, usuario, contraseña, cadena de conexión).
- Detener, reanudar y eliminar instancias y su VM subyacente.
- Reintentar instancias que fallaron durante el aprovisionamiento.
- Consultar un registro completo de actividad del servicio.

---

## Arquitectura del sistema

```
┌─────────────────────────────────────────────────────┐
│                  Host (tu máquina)                  │
│                                                     │
│   ┌───────────────────────────────────────────┐    │
│   │         NimbusDBaaS (Go, :8080)           │    │
│   │                                           │    │
│   │  ┌──────────┐   ┌──────────┐             │    │
│   │  │ HTTP API  │   │   Web    │             │    │
│   │  │  /api/*  │   │  /index  │             │    │
│   │  └────┬─────┘   └──────────┘             │    │
│   │       │                                   │    │
│   │  ┌────▼──────────────────────────────┐   │    │
│   │  │         Provisioner               │   │    │
│   │  │  • Clona VMs desde plantillas     │   │    │
│   │  │  • Llama a VBoxManage CLI         │   │    │
│   │  │  • Aprovisionamiento via SSH      │   │    │
│   │  │  • Health checks periódicos       │   │    │
│   │  └────┬──────────────────────────────┘   │    │
│   │       │                                   │    │
│   │  ┌────▼──────────┐                        │    │
│   │  │  JSON Store   │ (data/nimbus.db)       │    │
│   │  └───────────────┘                        │    │
│   └───────────────────────────────────────────┘    │
│                                                     │
│   ┌──────────────┐    ┌──────────────┐             │
│   │  VM: MariaDB │    │  VM: PgSQL   │  ...        │
│   │  (clonada)   │    │  (clonada)   │             │
│   │  192.168.10.X│    │  192.168.10.Y│             │
│   └──────────────┘    └──────────────┘             │
│           ▲                   ▲                     │
│           └─── host-only ─────┘                     │
└─────────────────────────────────────────────────────┘
```

### Componentes

| Componente | Tecnología | Responsabilidad |
|---|---|---|
| **Servidor HTTP** | Go `net/http` | Sirve la SPA y expone la API REST |
| **API REST** | Go | Endpoints para CRUD de instancias y logs |
| **Provisioner** | Go + VBoxManage CLI + `golang.org/x/crypto/ssh` | Clona VMs, asigna red, ejecuta SSH, health checks |
| **Store** | JSON (`encoding/json`) | Persiste instancias y logs en archivo plano (sin CGO) |
| **Frontend** | HTML + JS (Vanilla) | SPA con polling automático, drag & drop, animaciones |
| **VMs plantilla** | Debian 12 + MariaDB/PostgreSQL | Base inmutable por motor, preparada con snapshot `base` para linked clones |

---

## Estructura del proyecto

```
nimbusDBaaS/
├── cmd/
│   └── server/
│       └── main.go              # Punto de entrada
├── internal/
│   ├── api/
│   │   └── router.go            # Handlers HTTP y enrutamiento
│   ├── models/
│   │   └── models.go            # Structs: Instance, LogEntry, Engine, Status
│   ├── provisioner/
│   │   └── provisioner.go       # Lógica de VirtualBox, SSH y health checks
│   └── store/
│       └── store.go             # Persistencia en JSON (thread-safe)
├── web/
│   └── templates/
│       └── index.html           # SPA completa (HTML + CSS + JS)
├── scripts/
│   └── setup-templates.sh       # Script de configuración de VMs plantilla
├── data/                        # Creado en runtime (nimbus.db — JSON)
├── go.mod
├── go.sum
├── Makefile
└── README.md
```

---

## Requisitos previos

### Siempre necesarios
- **Go 1.21+** — [descargar](https://go.dev/dl/)
- Dependencia: `golang.org/x/crypto` (se descarga automáticamente con `go mod tidy`)

### Solo para modo real (VirtualBox)
- **Oracle VirtualBox 7.x** — [descargar](https://www.virtualbox.org/)
- `VBoxManage` disponible en el PATH
- VMs plantilla configuradas (ver sección correspondiente)
- Par de llaves SSH para el provisioner

---

## Instalación y ejecución

```bash
# 1. Clonar / descomprimir el proyecto
cd nimbusDBaaS

# 2. Descargar dependencias
go mod tidy

# 3a. Modo simulado (sin VirtualBox — ideal para desarrollo y demo)
make run-sim
# equivalente a: NIMBUS_SIMULATED=1 go run ./cmd/server/main.go

# 3b. Modo real (VirtualBox configurado)
make run

# 4. Abrir el navegador
#    http://localhost:8080
```

---

## Modo simulado vs. modo real

| Aspecto | Simulado (`NIMBUS_SIMULATED=1`) | Real (`NIMBUS_SIMULATED=0`) |
|---|---|---|
| VirtualBox requerido | No | Sí |
| VMs creadas | No | Sí (linked clone) |
| IPs asignadas | Aleatorias (192.168.10.X) | Reales (DHCP host-only) |
| SSH ejecutado | No | Sí |
| Base de datos real | No | Sí |
| Duración aprovisionamiento | ~6 segundos | ~1-2 minutos |
| Uso en presentación | Sí | Sí |

En modo simulado el flujo completo es visible en la UI (provisioning → running) con logs detallados, ideal para demostrar el funcionamiento sin infraestructura.

---

## Configuración de plantillas VirtualBox (modo real)

Ejecutar el script de setup (solo una vez):

```bash
chmod +x scripts/setup-templates.sh
./scripts/setup-templates.sh
```

El script:
1. Genera el par de llaves SSH en `~/.ssh/nimbus_id_rsa`.
2. Crea el adaptador host-only `vboxnet0`.
3. Crea dos VMs base distintas, una por motor.
4. Deja indicado el snapshot `base` para clonar con `clonevm --snapshot base --options link`.

### Pasos manuales en cada VM

**nimbus-mariadb-template:**
```bash
apt update && apt install -y mariadb-server openssh-server

# Habilitar acceso root con contraseña
mariadb -u root -e "ALTER USER 'root'@'localhost' IDENTIFIED BY 'root'; FLUSH PRIVILEGES;"

# Permitir conexiones remotas: /etc/mysql/mariadb.conf.d/50-server.cnf
# bind-address = 0.0.0.0

# Agregar llave pública del host
mkdir -p /root/.ssh
echo "<contenido de ~/.ssh/nimbus_id_rsa.pub>" >> /root/.ssh/authorized_keys
chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys

systemctl enable mariadb ssh
```

**nimbus-pg-template:**
```bash
apt update && apt install -y postgresql openssh-server

# /etc/postgresql/*/main/postgresql.conf → listen_addresses = '*'
# /etc/postgresql/*/main/pg_hba.conf → host all all 0.0.0.0/0 md5

mkdir -p /root/.ssh
echo "<contenido de ~/.ssh/nimbus_id_rsa.pub>" >> /root/.ssh/authorized_keys
chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys

systemctl enable postgresql ssh
```

**Tomar snapshot base y apagar:**
```bash
VBoxManage snapshot nimbus-mariadb-template take base --pause
VBoxManage controlvm nimbus-mariadb-template poweroff

VBoxManage snapshot nimbus-pg-template take base --pause
VBoxManage controlvm nimbus-pg-template poweroff
```

---

## Variables de entorno

| Variable | Valor por defecto | Descripción |
|---|---|---|
| `NIMBUS_SIMULATED` | `1` | `1` = simulado, `0` = real |
| `NIMBUS_MARIADB_TEMPLATE` | `nimbus-mariadb-template` | Nombre de la VM plantilla MariaDB |
| `NIMBUS_PG_TEMPLATE` | `nimbus-pg-template` | Nombre de la VM plantilla PostgreSQL |
| `NIMBUS_TEMPLATE_SNAPSHOT` | `base` | Snapshot usado como origen del linked clone |
| `NIMBUS_TEMPLATE_ISO` | `~/Downloads/debian-13.4.0-amd64-netinst.iso` | Ruta local de la ISO Debian |
| `NIMBUS_TEMPLATE_ISO_URL` | `https://cdimage.debian.org/debian-cd/current/amd64/iso-cd/` | URL de descarga de ISO Debian |
| `NIMBUS_TEMPLATE_USER` | `root` | Usuario SSH usado por el provisioner |
| `NIMBUS_HOST_ONLY_NET` | `VirtualBox Host-Only Ethernet Adapter` | Nombre del adaptador host-only |
| `NIMBUS_SSH_KEY` | `~/.ssh/id_rsa` | Ruta a la llave privada SSH |
| `NIMBUS_BASE_IP` | `192.168.10` | Prefijo de red para IPs |
| `VBOXMANAGE` | `VBoxManage` | Ruta al ejecutable VBoxManage |
| `PORT` | `8080` | Puerto donde escucha el servidor HTTP |

---

## API REST

### `GET /api/instances`
Lista todas las instancias activas (excluye eliminadas).

**Respuesta:**
```json
[
  {
    "id": "uuid",
    "db_name": "Clientes",
    "username": "carlos",
    "password": "s0qf1M4abc",
    "engine": "mariadb",
    "status": "running",
    "host": "192.168.10.42",
    "port": 3306,
    "vm_name": "nimbus-mariadb-a1b2c3d4",
    "access_cmd": "mariadb -h 192.168.10.42 -u carlos -ps0qf1M4abc Clientes",
    "created_at": "2026-05-15T10:05:00Z"
  }
]
```

### `POST /api/instances`
Crea una nueva instancia (responde 202 Accepted, aprovisionamiento async).

**Body JSON:**
```json
{
  "db_name": "Clientes",
  "username": "carlos",
  "engine": "mariadb",
  "sql_content": "CREATE TABLE productos (...);"
}
```

O **multipart/form-data** con campos `db_name`, `username`, `engine` y archivo `sql_file`.

### `GET /api/instances/:id`
Obtiene una instancia por ID (útil para polling de estado).

### `DELETE /api/instances/:id`
Elimina una instancia (apaga y borra la VM en background).

### `POST /api/instances/:id/stop`
Detiene la VM de una instancia (ACPI poweroff con force-fallback).

### `POST /api/instances/:id/resume`
Reanuda una instancia detenida.

### `POST /api/instances/:id/retry`
Reintenta el aprovisionamiento de una instancia en estado `error`. Limpia la VM fallida y vuelve a ejecutar el flujo completo.

### `DELETE /api/instances/:id/logs`
Elimina los registros de log asociados a una instancia.

### `GET /api/logs`
Devuelve todos los registros de actividad (hasta 200, orden cronológico).

### `GET /api/config`
Expone la configuración activa del provisioner (plantillas, red, SSH, ISO).

---

## Flujo de aprovisionamiento

```
Usuario llena formulario
        │
        ▼
POST /api/instances
        │
        ├─ Valida campos (incluye validación de identificadores contra palabras reservadas)
        ├─ Genera UUID, contraseña aleatoria
        ├─ Persiste en JSON Store (status: provisioning)
        └─ Lanza goroutine de aprovisionamiento
                │
                ├─ Log: "Solicitud de creación…"
                ├─ Valida nombre de BD y usuario contra regex
                ├─ Asigna nombre de VM
                ├─ [Real] Reserva IP disponible (rango .20-.254, TTL 5 min)
                ├─ [Real] Verificar o bootstrappear plantilla base + snapshot
                ├─ [Real] Clonar VM (VBoxManage clonevm --snapshot base --options link)
                ├─ [Real] Iniciar VM (VBoxManage startvm --type headless)
                ├─ [Real] Asignar IP vía SSH a la plantilla y esperar conectividad
                ├─ [Real] SSH: CREATE DATABASE, CREATE USER, GRANT ALL PRIVILEGES
                ├─ [Real] SSH: ejecutar archivo .sql (si se proporcionó, sanitizado)
                ├─ Actualizar Store (status: running, host, puerto, access_cmd)
                └─ Log: "Instancia lista — host asignado X.X.X.X"
```

### Health checks

El provisioner ejecuta health checks cada 10 segundos sobre las VMs activas. Si una VM no responde 3 veces consecutivas, se marca como `stopped`. Si vuelve a aparecer, se reanuda automáticamente.

### Reintento

Si una instancia falla durante el aprovisionamiento (status `error`), se puede reintentar vía `POST /api/instances/:id/retry`. El sistema limpia la VM fallida y ejecuta el flujo completo desde cero.

---

## Interfaz de usuario

La interfaz es una SPA (Single Page Application) construida con HTML, CSS y JavaScript vanilla.

| Sección | Descripción |
|---|---|
| **Topbar** | Logo NimbusDBaaS, indicador de entorno, avatar |
| **Sidebar** | Navegación: Crear BD, Instancias, Registros |
| **Crear BD** | Formulario con nombre, usuario, selector de motor (MariaDB / PostgreSQL), upload de `.sql` con drag & drop |
| **Instancias activas** | Cards con badge del motor, estado animado, credenciales de acceso con botones de copia, acciones (detener/reanudar/reintentar/eliminar) |
| **Registro de actividad** | Logs con timestamp, nivel (INFO/OK/ERROR) y mensaje |

### Características

- **Polling adaptativo**: cada 3 segundos mientras hay instancias en `provisioning`, cada 10 segundos cuando todas están estables
- **Toasts** de notificación para acciones (creación, eliminación, errores)
- **Modal de confirmación** para eliminación de instancias
- **Modal de acción** para confirmar detener/reanudar/reintentar
- **Botones de copiar** para host, contraseña y cadena de acceso
- **Animaciones**: spinner en aprovisionamiento, pulso en estado, barrido en acciones, aparición de tarjetas
- **Drag & drop** para el archivo SQL
- **Renderizado por diff**: solo actualiza las cards que cambiaron (minimiza manipulación del DOM)

---

*NimbusDBaaS — Computación en la nube 2026-1*
