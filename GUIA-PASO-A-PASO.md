# NimbusDBaaS — Guía Paso a Paso

**Prototipo funcional de un servicio administrado de bases de datos (DBaaS)**  
*Computación en la nube 2026-1 — Nota Parcial #3*

---

## Índice

1. [Descripción del proyecto](#1-descripción-del-proyecto)
2. [Tecnologías utilizadas](#2-tecnologías-utilizadas)
3. [Arquitectura del sistema](#3-arquitectura-del-sistema)
4. [Requisitos previos](#4-requisitos-previos)
5. [Instalación paso a paso](#5-instalación-paso-a-paso)
6. [Ejecución en modo simulado](#6-ejecución-en-modo-simulado)
7. [Ejecución en modo real (con VirtualBox)](#7-ejecución-en-modo-real-con-virtualbox)
8. [Uso del sistema](#8-uso-del-sistema)
9. [API REST](#9-api-rest)
10. [Solución de problemas](#10-solución-de-problemas)

---

## 1. Descripción del proyecto

NimbusDBaaS es un prototipo de **Database as a Service** que imita el comportamiento de servicios como Amazon RDS o Google Cloud SQL, pero ejecutado **completamente en local** usando Oracle VirtualBox.

El sistema permite:

- **Crear** instancias de bases de datos MariaDB o PostgreSQL desde un formulario web.
- **Ver** las instancias activas con credenciales (host, puerto, usuario, contraseña).
- **Detener, reanudar y eliminar** instancias.
- **Reintentar** instancias que fallaron durante el aprovisionamiento.
- **Consultar** un registro de actividad completo.

> **📸 Foto del sistema funcionando**
>
> _[Agregar aquí una captura de pantalla del panel principal de NimbusDBaaS mostrando el listado de instancias activas]_

---

## 2. Tecnologías utilizadas

### Backend

| Tecnología | Versión | Uso |
|---|---|---|
| **Go** | 1.25 | Lenguaje principal del servidor |
| **net/http** | (stdlib) | Enrutamiento y handlers HTTP |
| **encoding/json** | (stdlib) | Persistencia en archivo JSON |
| **golang.org/x/crypto** | v0.52.0 | Cliente SSH para aprovisionar VMs |
| **VBoxManage CLI** | — | Interacción con VirtualBox (clonado, inicio, red) |

### Frontend

| Tecnología | Uso |
|---|---|
| **HTML5** | Estructura de la SPA |
| **CSS3** | Estilos, animaciones, layout responsive |
| **JavaScript (Vanilla)** | Lógica del cliente, polling, drag & drop |

### Infraestructura

| Tecnología | Uso |
|---|---|
| **Oracle VirtualBox 7.x** | Hipervisor para VMs |
| **Debian 12** | SO de las VMs plantilla |
| **MariaDB** | Motor de base de datos soportado |
| **PostgreSQL** | Motor de base de datos soportado |
| **Host-Only Networking** | Red aislada entre host y VMs |
| **Linked Clones** | Clonado eficiente de VMs con snapshots |

### Herramientas de desarrollo

| Herramienta | Uso |
|---|---|
| **Make** | Automatización de builds y ejecución |
| **Go Modules** | Gestión de dependencias |

> **📸 Logo / tech stack visual**
>
> _[Agregar aquí una imagen con los logos de Go, MariaDB, PostgreSQL, VirtualBox]_

---

## 3. Arquitectura del sistema

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

> **📸 Diagrama de arquitectura**
>
> _[Agregar aquí una versión más detallada o visual del diagrama de arquitectura]_

### Flujo de aprovisionamiento

```
Usuario llena formulario
        │
        ▼
POST /api/instances
        │
        ├─ Valida campos
        ├─ Genera UUID, contraseña aleatoria
        ├─ Persiste en JSON Store (status: provisioning)
        └─ Lanza goroutine de aprovisionamiento
                │
                ├─ [Real] Reserva IP disponible
                ├─ [Real] Clona VM desde snapshot base
                ├─ [Real] Inicia VM (headless)
                ├─ [Real] Configura red y espera conectividad SSH
                ├─ [Real] Crea BD, usuario y grants via SSH
                ├─ [Real] Ejecuta script SQL opcional
                ├─ Actualiza Store (status: running)
                └─ Log: "Instancia lista"
```

---

## 4. Requisitos previos

### Para todos los modos

| Requisito | Versión mínima | Dónde obtenerlo |
|---|---|---|
| **Go** | 1.21+ | [https://go.dev/dl/](https://go.dev/dl/) |
| **Git** | — | [https://git-scm.com/](https://git-scm.com/) |

### Solo para modo real (con VirtualBox)

| Requisito | Versión | Dónde obtenerlo |
|---|---|---|
| **Oracle VirtualBox** | 7.x | [https://www.virtualbox.org/](https://www.virtualbox.org/) |
| **VBoxManage** | (viene con VBox) | Asegurarse que esté en el PATH |
| **ISO de Debian 12** | 12.x | [https://cdimage.debian.org/](https://cdimage.debian.org/debian-cd/current/amd64/iso-cd/) |
| **Par de llaves SSH** | — | Se genera con `scripts/setup-templates.sh` |

> **📸 Verificación de requisitos**
>
> _[Agregar aquí capturas de pantalla mostrando `go version`, `VBoxManage --version`, etc.]_

---

## 5. Instalación paso a paso

### Paso 1: Clonar el repositorio

```bash
git clone <url-del-repositorio> nimbusDBaaS
cd nimbusDBaaS
```

> **📸 Clonación exitosa**
>
> _[Agregar aquí captura de pantalla del terminal mostrando `git clone` completado]_

### Paso 2: Explorar la estructura del proyecto

```bash
# Ver el árbol de directorios
tree -L 2 -I '.git|data'
```

```
nimbusDBaaS/
├── cmd/server/main.go       # Punto de entrada
├── internal/
│   ├── api/router.go        # Handlers HTTP
│   ├── models/models.go     # Structs del dominio
│   ├── provisioner/         # Lógica de VirtualBox + SSH
│   └── store/store.go       # Persistencia JSON
├── web/templates/index.html # SPA frontend
├── scripts/setup-templates.sh
├── go.mod / go.sum
├── Makefile
└── README.md
```

> **📸 Estructura del proyecto**
>
> _[Agregar aquí captura de pantalla del árbol de directorios]_

### Paso 3: Descargar dependencias

```bash
go mod tidy
```

Esto descarga `golang.org/x/crypto` necesaria para la conexión SSH.

> **📸 Dependencias instaladas**
>
> _[Agregar aquí captura del `go mod tidy` ejecutándose sin errores]_

### Paso 4: Compilar (opcional)

```bash
make build
```

> **📸 Build exitoso**
>
> _[Agregar aquí captura del `make build` completado]**

---

## 6. Ejecución en modo simulado

Este modo **no requiere VirtualBox**. Es ideal para desarrollo, pruebas y demostraciones.

```bash
make run-sim
# Equivalente a: NIMBUS_SIMULATED=1 go run ./cmd/server/main.go
```

La salida debería ser similar a:

```
2026/05/24 10:00:00 ▶ NimbusDBaaS — modo SIMULADO
2026/05/24 10:00:00 ▶ Servidor escuchando en :8080
```

> **📸 Servidor iniciado en modo simulado**
>
> _[Agregar aquí captura de pantalla del terminal mostrando el servidor corriendo]_

### Abrir el navegador

Abrí [http://localhost:8080](http://localhost:8080) en tu navegador.

> **📸 Pantalla de inicio**
>
> _[Agregar aquí captura de pantalla de la interfaz web de NimbusDBaaS]_

---

## 7. Ejecución en modo real (con VirtualBox)

### Paso 1: Configurar las VMs plantilla

Ejecutá el script de setup:

```bash
chmod +x scripts/setup-templates.sh
./scripts/setup-templates.sh
```

Este script:
1. Genera el par de llaves SSH en `~/.ssh/nimbus_id_rsa`.
2. Crea el adaptador host-only `vboxnet0`.
3. Crea dos VMs base (MariaDB y PostgreSQL) con Debian 12.
4. Toma el snapshot `base` para linked clones.

> **📸 Script de setup ejecutándose**
>
> _[Agregar aquí captura del script setup corriendo]_

### Paso 2: Configuración manual en cada VM

**Para nimbus-mariadb-template:**

```bash
# Acceder por SSH a la VM y ejecutar:
apt update && apt install -y mariadb-server openssh-server

# Configurar MariaDB
mariadb -u root -e "ALTER USER 'root'@'localhost' IDENTIFIED BY 'root'; FLUSH PRIVILEGES;"

# Permitir conexiones remotas (editar 50-server.cnf):
# bind-address = 0.0.0.0

# Agregar clave pública del host
mkdir -p /root/.ssh
echo "<contenido de ~/.ssh/nimbus_id_rsa.pub>" >> /root/.ssh/authorized_keys
chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys

systemctl enable mariadb ssh
```

**Para nimbus-pg-template:**

```bash
apt update && apt install -y postgresql openssh-server

# Configurar PostgreSQL:
# postgresql.conf → listen_addresses = '*'
# pg_hba.conf → host all all 0.0.0.0/0 md5

mkdir -p /root/.ssh
echo "<contenido de ~/.ssh/nimbus_id_rsa.pub>" >> /root/.ssh/authorized_keys
chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys

systemctl enable postgresql ssh
```

> **📸 Configuración de plantillas**
>
> _[Agregar aquí captura de la VM plantilla con MariaDB instalado]_

### Paso 3: Tomar snapshot base

```bash
VBoxManage snapshot nimbus-mariadb-template take base --pause
VBoxManage controlvm nimbus-mariadb-template poweroff

VBoxManage snapshot nimbus-pg-template take base --pause
VBoxManage controlvm nimbus-pg-template poweroff
```

### Paso 4: Iniciar el servidor

```bash
make run
# Equivalente a: NIMBUS_SIMULATED=0 go run ./cmd/server/main.go
```

> **📸 Servidor corriendo en modo real**
>
> _[Agregar aquí captura del terminal mostrando el servidor en modo real]_

---

## 8. Uso del sistema

### 8.1 Crear una base de datos

1. En la barra lateral, hacé clic en **"Crear BD"**.
2. Completá los campos:
   - **Nombre de la BD**: ej. `Clientes`
   - **Usuario administrador**: ej. `carlos`
   - **Motor**: seleccioná `MariaDB` o `PostgreSQL`.
   - **(Opcional)** Subí un archivo `.sql` con tablas iniciales.
3. Hacé clic en **"Crear instancia"**.

> **📸 Formulario de creación**
>
> _[Agregar aquí captura de pantalla del formulario de creación lleno]_

### 8.2 Ver instancias activas

Las instancias aparecen como tarjetas con:
- Badge del motor (MariaDB / PostgreSQL)
- Estado animado (provisioning → running)
- Host, puerto, usuario y contraseña
- Cadena de conexión lista para copiar
- Botones de acción: detener, reanudar, reintentar, eliminar

> **📸 Instancias activas**
>
> _[Agregar aquí captura de pantalla mostrando una o más instancias en estado "running"]_

### 8.3 Conectarse a la base de datos

Usando la cadena de conexión que aparece en la tarjeta:

```bash
# MariaDB
mariadb -h 192.168.10.42 -u carlos -ps0qf1M4abc Clientes

# PostgreSQL
psql -h 192.168.10.43 -U carlos -d Clientes
```

También podés copiar el host, usuario o contraseña individualmente con los botones de copiar.

> **📸 Conexión desde terminal**
>
> _[Agregar aquí captura de pantalla mostrando una conexión exitosa a la BD desde la terminal]_

### 8.4 Detener y reanudar instancias

- **Detener**: apaga la VM (ACPI poweroff).
- **Reanudar**: vuelve a iniciar la VM.
- **Reintentar**: para instancias en estado `error`, limpia la VM fallida y vuelve a aprovisionar.

> **📸 Acciones sobre instancias**
>
> _[Agregar aquí captura mostrando el modal de confirmación para detener/eliminar]_

### 8.5 Ver registro de actividad

En la sección **"Registros"** de la barra lateral se muestra el historial completo con:
- Timestamp
- Nivel (INFO / OK / ERROR)
- Mensaje descriptivo
- ID de instancia asociada (si corresponde)

> **📸 Registro de actividad**
>
> _[Agregar aquí captura de pantalla del log de actividad]_

---

## 9. API REST

El sistema expone una API RESTful en `http://localhost:8080/api/`.

| Método | Endpoint | Descripción |
|---|---|---|
| `GET` | `/api/instances` | Lista todas las instancias activas |
| `POST` | `/api/instances` | Crea una nueva instancia (async) |
| `GET` | `/api/instances/:id` | Obtiene una instancia por ID |
| `DELETE` | `/api/instances/:id` | Elimina una instancia |
| `POST` | `/api/instances/:id/stop` | Detiene una instancia |
| `POST` | `/api/instances/:id/resume` | Reanuda una instancia detenida |
| `POST` | `/api/instances/:id/retry` | Reintenta una instancia fallida |
| `DELETE` | `/api/instances/:id/logs` | Elimina logs de una instancia |
| `GET` | `/api/logs` | Devuelve todos los registros |
| `GET` | `/api/config` | Muestra configuración del provisioner |

### Ejemplo de creación (curl)

```bash
curl -X POST http://localhost:8080/api/instances \
  -H "Content-Type: application/json" \
  -d '{
    "db_name": "Clientes",
    "username": "carlos",
    "engine": "mariadb"
  }'
```

Respuesta (`202 Accepted`):

```json
{
  "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "status": "provisioning",
  "db_name": "Clientes"
}
```

> **📸 Prueba con curl / Postman**
>
> _[Agregar aquí captura de una prueba de la API con curl o Postman]_

---

## 10. Solución de problemas

### Error: "VBoxManage not found"

Asegurate de que VirtualBox esté instalado y `VBoxManage` esté en el PATH:

```bash
which VBoxManage
# Si no aparece, agregalo al PATH o usá la variable de entorno:
export VBOXMANAGE="/ruta/a/VBoxManage"
```

### Error: "SSH connection refused"

Verificá que la VM esté encendida y tenga la IP correcta:

```bash
VBoxManage list runningvms
VBoxManage guestproperty get <vm-name> /VirtualBox/GuestInfo/Net/0/V4/IP
```

### Error: "Port already in use"

Si el puerto 8080 ya está ocupado, usá otro puerto:

```bash
PORT=9090 make run-sim
```

### Error de dependencias

```bash
go clean -modcache && go mod tidy
```

> **📸 Error común y solución**
>
> _[Agregar aquí captura de un error típico y cómo resolverlo]_

---

## Variables de entorno

| Variable | Default | Descripción |
|---|---|---|
| `NIMBUS_SIMULATED` | `1` | `1`=simulado, `0`=real |
| `PORT` | `8080` | Puerto del servidor HTTP |
| `NIMBUS_MARIADB_TEMPLATE` | `nimbus-mariadb-template` | Nombre VM plantilla MariaDB |
| `NIMBUS_PG_TEMPLATE` | `nimbus-pg-template` | Nombre VM plantilla PostgreSQL |
| `NIMBUS_SSH_KEY` | `~/.ssh/id_rsa` | Ruta llave privada SSH |
| `NIMBUS_BASE_IP` | `192.168.10` | Prefijo de red |
| `VBOXMANAGE` | `VBoxManage` | Ruta al ejecutable VBoxManage |

---

*NimbusDBaaS — Computación en la nube 2026-1*  
*Documentación generada en mayo 2026*
