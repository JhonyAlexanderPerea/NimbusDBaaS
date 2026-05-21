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
10. [Flujo de provisionamiento](#flujo-de-provisionamiento)
11. [Diseño de la interfaz (mockup)](#diseño-de-la-interfaz)

---

## Descripción general

NimbusDBaaS es un prototipo de *Database as a Service* que simula el comportamiento de servicios como Amazon RDS o Google Cloud SQL, pero ejecutado completamente en un entorno local con Oracle VirtualBox.

El sistema permite a los usuarios:

- Crear instancias de bases de datos MariaDB o PostgreSQL con un solo formulario web.
- Especificar el nombre de la base de datos, el usuario administrador y (opcionalmente) un archivo `.sql` de inicialización.
- Ver las instancias activas con sus credenciales de acceso (host, puerto, usuario, contraseña, cadena de conexión para DBeaver).
- Eliminar instancias y su VM subyacente.
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
│   │  │  • Provisionamiento async via SSH │   │    │
│   │  └────┬──────────────────────────────┘   │    │
│   │       │                                   │    │
│   │  ┌────▼──────────┐                        │    │
│   │  │  SQLite Store │ (data/nimbus.db)       │    │
│   │  └───────────────┘                        │    │
│   └───────────────────────────────────────────┘    │
│                                                     │
│   ┌──────────────┐    ┌──────────────┐             │
│   │  VM: MariaDB │    │  VM: PgSQL   │  ...        │
│   │  (clonada)   │    │  (clonada)   │             │
│   │  192.168.56.X│    │  192.168.56.Y│             │
│   └──────────────┘    └──────────────┘             │
│           ▲                   ▲                     │
│           └───── vboxnet0 ────┘                     │
└─────────────────────────────────────────────────────┘
```

### Componentes

| Componente | Tecnología | Responsabilidad |
|---|---|---|
| **Servidor HTTP** | Go `net/http` | Sirve la SPA y expone la API REST |
| **API REST** | Go | Endpoints para CRUD de instancias y logs |
| **Provisioner** | Go + VBoxManage CLI | Clona VMs, asigna red, lanza SSH |
| **Store** | SQLite (`go-sqlite3`) | Persiste instancias y logs |
| **Frontend** | HTML + JS (Vanilla) | SPA fiel al mockup, polling automático |
| **VMs plantilla** | Debian 12 + MariaDB/PostgreSQL | Base inmutable para cada motor |

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
│   │   └── models.go            # Structs: Instance, LogEntry, etc.
│   ├── provisioner/
│   │   └── provisioner.go       # Lógica de VirtualBox y SSH
│   └── store/
│       └── store.go             # Capa SQLite (instancias + logs)
├── web/
│   └── templates/
│       └── index.html           # SPA completa (HTML + CSS + JS)
├── scripts/
│   └── setup-templates.sh       # Script de configuración de VMs plantilla
├── data/                        # Creado en runtime (nimbus.db)
├── go.mod
├── go.sum
├── Makefile
└── README.md
```

---

## Requisitos previos

### Siempre necesarios
- **Go 1.21+** — [descargar](https://go.dev/dl/)
- **GCC / build-essential** — requerido por `go-sqlite3` (CGO)
  ```bash
  # Ubuntu/Debian
  sudo apt install -y build-essential
  # macOS
  xcode-select --install
  ```

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
| IPs asignadas | Aleatorias (192.168.56.X) | Reales (DHCP host-only) |
| SSH ejecutado | No | Sí |
| Base de datos real | No | Sí |
| Duración provisionamiento | ~6 segundos | ~1-2 minutos |
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
3. Crea dos VMs base (una para MariaDB, otra para PostgreSQL).
4. Muestra los pasos manuales de instalación de Debian y los motores.

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
| `NIMBUS_HOST_ONLY_NET` | `vboxnet0` | Nombre del adaptador host-only |
| `NIMBUS_SSH_KEY` | `~/.ssh/nimbus_id_rsa` | Ruta a la llave privada SSH |
| `NIMBUS_BASE_IP` | `192.168.56` | Prefijo de red para IPs simuladas |
| `VBOXMANAGE` | `VBoxManage` | Ruta al ejecutable VBoxManage |

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
    "host": "192.168.56.42",
    "port": 3306,
    "vm_name": "nimbus-mariadb-a1b2c3d4",
    "access_cmd": "mariadb -h 192.168.56.42 -u carlos -ps0qf1M4abc Clientes",
    "created_at": "2026-05-15T10:05:00Z"
  }
]
```

### `POST /api/instances`
Crea una nueva instancia (responde 202 Accepted, el provisionamiento es async).

**Body JSON:**
```json
{
  "db_name": "Clientes",
  "username": "carlos",
  "engine": "mariadb",
  "sql_content": "CREATE TABLE productos (...);"
}
```

**O multipart/form-data** con campos `db_name`, `username`, `engine` y archivo `sql_file`.

### `GET /api/instances/:id`
Obtiene una instancia por ID (útil para polling de estado).

### `DELETE /api/instances/:id`
Elimina una instancia (apaga y borra la VM en background).

### `GET /api/logs`
Devuelve todos los registros de actividad (hasta 200, orden cronológico).

---

## Flujo de provisionamiento

```
Usuario llena formulario
        │
        ▼
POST /api/instances
        │
        ├─ Valida campos
        ├─ Genera UUID, contraseña aleatoria
        ├─ Persiste en SQLite (status: provisioning)
        └─ Lanza goroutine de provisionamiento
                │
                ├─ Log: "Solicitud de creación…"
                ├─ [Real] Clonar VM (VBoxManage clonevm --options link)
                ├─ [Real] Configurar red host-only
                ├─ [Real] Iniciar VM (VBoxManage startvm --type headless)
                ├─ [Real] Esperar IP (guestproperty polling)
                ├─ Actualizar Store con host/puerto
                ├─ Log: "Creación de MV exitosa"
                ├─ [Real] SSH: CREATE DATABASE …
                ├─ [Real] SSH: CREATE USER …
                ├─ [Real] SSH: GRANT ALL PRIVILEGES …
                ├─ [Real] SSH: ejecutar archivo .sql (si se proporcionó)
                ├─ Actualizar Store (status: running)
                └─ Log: "Instancia lista — host asignado X.X.X.X"
```

La UI hace polling cada 3 segundos mientras haya instancias en estado `provisioning`, actualizando las tarjetas automáticamente hasta que cambian a `running`.

---

## Diseño de la interfaz

La interfaz fue implementada siguiendo fielmente el mockup aprobado:

| Sección | Descripción |
|---|---|
| **Topbar** | Logo NimbusDBaaS, indicador de entorno, avatar |
| **Sidebar** | Navegación: Bases de datos, Instancias, Registros, Configuración |
| **Crear BD** | Formulario con nombre, usuario, selector de motor (MariaDB / PostgreSQL), upload de .sql |
| **Instancias activas** | Cards con badge del motor, estado animado, credenciales de acceso con botones de copia, botón eliminar |
| **Registro de actividad** | Logs con timestamp, nivel (INFO/OK/ERROR) y mensaje |
| **Configuración** | Variables de entorno activas y estado del modo (simulado/real) |

Características adicionales respecto al mockup estático:
- **Polling automático** de instancias mientras se provisionan
- **Toasts** de notificación para acciones
- **Modal de confirmación** para eliminar instancias
- **Botones de copiar** para host, contraseña y cadena de acceso
- **Animaciones** de estado (spinner en provisioning, pulso en punto de estado)
- **Drag & drop** para el archivo SQL

---

*NimbusDBaaS — Computación en la nube 2026-1*
