#!/usr/bin/env bash
# =============================================================================
# NimbusDBaaS – Setup de plantillas VirtualBox
# Ejecutar UNA SOLA VEZ en el host Windows (Git Bash / WSL / PowerShell no aplica)
# Para Windows, ejecutar los comandos VBoxManage en PowerShell manualmente.
# =============================================================================
set -euo pipefail

# ── Configuración ──────────────────────────────────────────────────────────
MARIADB_TEMPLATE="nimbus-mariadb-template"
PG_TEMPLATE="nimbus-pg-template"
HOST_ONLY_ADAPTER="VirtualBox Host-Only Ethernet Adapter"   # Nombre en Windows
HOST_ONLY_IP="192.168.10.1"
MARIADB_IP="192.168.10.10"
PG_IP="192.168.10.11"
SSH_KEY="$HOME/.ssh/id_rsa"
SNAPSHOT_NAME="base"

echo "=================================================="
echo "  NimbusDBaaS — Setup de plantillas VirtualBox"
echo "=================================================="
echo ""

# ── 0. Verificar llave SSH ─────────────────────────────────────────────────
echo "[1/4] Verificando llave SSH en $SSH_KEY …"
if [ ! -f "$SSH_KEY" ]; then
  echo "      No encontrada. Generando par de llaves RSA…"
  mkdir -p "$HOME/.ssh"
  ssh-keygen -t rsa -b 4096 -f "$SSH_KEY" -N "" -C "nimbus-dbaas"
  echo "      ✓ Llave generada: $SSH_KEY"
else
  echo "      ✓ Llave ya existe: $SSH_KEY"
fi
PUB_KEY=$(cat "${SSH_KEY}.pub")
echo ""
echo "  ╔══════════════════════════════════════════════════════════════╗"
echo "  ║  LLAVE PÚBLICA (copia esto en cada VM → /root/.ssh/authorized_keys) ║"
echo "  ╚══════════════════════════════════════════════════════════════╝"
echo "  $PUB_KEY"
echo ""

# ── 1. Verificar adaptador Host-Only ──────────────────────────────────────
echo "[2/4] Verificando adaptador Host-Only…"
if VBoxManage list hostonlyifs | grep -q "192.168.10.1"; then
  echo "      ✓ Red 192.168.10.0/24 ya configurada"
else
  echo "      Configurando adaptador host-only con IP $HOST_ONLY_IP…"
  # En Windows el adaptador ya existe, solo hay que configurarlo
  VBoxManage hostonlyif ipconfig "$HOST_ONLY_ADAPTER" \
    --ip "$HOST_ONLY_IP" --netmask 255.255.255.0 2>/dev/null || \
  echo "      ⚠ Configura manualmente en VirtualBox > Archivo > Administrador de red de anfitrión"
fi
echo ""

# ── 2. Verificar plantillas ────────────────────────────────────────────────
echo "[3/4] Verificando plantillas en VirtualBox…"

check_template() {
  local NAME="$1"
  local IP="$2"
  local ENGINE="$3"

  echo ""
  echo "  ┌─ Plantilla: $NAME ─────────────────────────────"
  if VBoxManage list vms | grep -q "\"$NAME\""; then
    echo "  │  ✓ VM existe en VirtualBox"

    # Verificar snapshot
    if VBoxManage snapshot "$NAME" list --machinereadable 2>/dev/null | grep -q "SnapshotName=\"$SNAPSHOT_NAME\""; then
      echo "  │  ✓ Snapshot '$SNAPSHOT_NAME' existe"
    else
      echo "  │  ✗ Falta snapshot '$SNAPSHOT_NAME'"
      echo "  │    Crea el snapshot con la VM APAGADA:"
      echo "  │    VBoxManage snapshot \"$NAME\" take \"$SNAPSHOT_NAME\""
    fi

    echo "  │  ✓ Arquitectura correcta: snapshot '$SNAPSHOT_NAME' + clonevm --options link"
  else
    echo "  │  ✗ VM NO existe en VirtualBox"
    echo "  │"
    echo "  │  Pasos para crear la plantilla $NAME:"
    echo "  │"
    echo "  │  1. Crear VM en VirtualBox:"
    echo "  │     - Nombre: $NAME"
    echo "  │     - Tipo: Linux, Debian (64-bit)"
    echo "  │     - RAM: 1024 MB"
    echo "  │     - Disco: 10 GB (VDI, dinámico)"
    echo "  │     - Red adaptador 1: NAT"
    echo "  │     - Red adaptador 2: Host-Only → $HOST_ONLY_ADAPTER"
    echo "  │"
    echo "  │  2. Instalar Debian 13 (CLI, sin escritorio)"
    echo "  │     - Si falla el mirror durante la instalación, omítelo"
    echo "  │     - Después corrige /etc/apt/sources.list:"
    echo "  │       deb http://deb.debian.org/debian trixie main non-free-firmware"
    echo "  │       deb http://security.debian.org/debian-security trixie-security main non-free-firmware"
    echo "  │"
    echo "  │  3. Dentro de la VM (como root):"
    echo "  │"
    if [ "$ENGINE" = "mariadb" ]; then
      echo "  │     # Configurar IP fija"
      echo "  │     # En /etc/network/interfaces agregar para enp0s8 (Host-Only):"
      echo "  │     auto enp0s8"
      echo "  │     iface enp0s8 inet static"
      echo "  │       address $IP"
      echo "  │       netmask 255.255.255.0"
      echo "  │"
      echo "  │     systemctl restart networking"
      echo "  │"
      echo "  │     # Instalar servicios"
      echo "  │     apt update && apt install -y openssh-server mariadb-server"
      echo "  │"
      echo "  │     # Configurar MariaDB para acceso remoto"
      echo "  │     sed -i 's/bind-address.*/bind-address = 0.0.0.0/' /etc/mysql/mariadb.conf.d/50-server.cnf"
      echo "  │     systemctl enable mariadb"
      echo "  │     systemctl restart mariadb"
    else
      echo "  │     # Configurar IP fija"
      echo "  │     # En /etc/network/interfaces agregar para enp0s8 (Host-Only):"
      echo "  │     auto enp0s8"
      echo "  │     iface enp0s8 inet static"
      echo "  │       address $IP"
      echo "  │       netmask 255.255.255.0"
      echo "  │"
      echo "  │     systemctl restart networking"
      echo "  │"
      echo "  │     # Instalar servicios"
      echo "  │     apt update && apt install -y openssh-server postgresql"
      echo "  │"
      echo "  │     # Configurar PostgreSQL para acceso remoto"
      echo "  │     PG_CONF=\$(ls /etc/postgresql/*/main/postgresql.conf | head -1)"
      echo "  │     PG_HBA=\$(ls /etc/postgresql/*/main/pg_hba.conf | head -1)"
      echo "  │     sed -i \"s/#listen_addresses.*/listen_addresses = '*'/\" \"\$PG_CONF\""
      echo "  │     echo 'host all all 0.0.0.0/0 md5' >> \"\$PG_HBA\""
      echo "  │     systemctl enable postgresql"
      echo "  │     systemctl restart postgresql"
      echo "  │"
      echo "  │     # Dar contraseña al usuario postgres"
      echo "  │     su - postgres -c \"psql -c \\\"ALTER USER postgres WITH PASSWORD 'postgres';\\\"\""
    fi
    echo "  │"
    echo "  │     # Configurar SSH: habilitar acceso root con llave"
    echo "  │     sed -i 's/#PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config"
    echo "  │     sed -i 's/#PubkeyAuthentication.*/PubkeyAuthentication yes/' /etc/ssh/sshd_config"
    echo "  │     sed -i 's/PasswordAuthentication yes/PasswordAuthentication no/' /etc/ssh/sshd_config"
    echo "  │     systemctl enable ssh"
    echo "  │     systemctl restart ssh"
    echo "  │"
    echo "  │     # Agregar llave pública del host"
    echo "  │     mkdir -p /root/.ssh && chmod 700 /root/.ssh"
    echo "  │     echo '$PUB_KEY' >> /root/.ssh/authorized_keys"
    echo "  │     chmod 600 /root/.ssh/authorized_keys"
    echo "  │"
    echo "  │  4. Apagar la VM y tomar snapshot:"
    echo "  │     VBoxManage controlvm \"$NAME\" poweroff"
    echo "  │     VBoxManage snapshot \"$NAME\" take \"$SNAPSHOT_NAME\""
    echo "  │"
    echo "  │  5. No configures multiattach: NimbusDBaaS usa linked clones desde el snapshot '$SNAPSHOT_NAME'."
  fi
  echo "  └──────────────────────────────────────────────────"
}

check_template "$MARIADB_TEMPLATE" "$MARIADB_IP" "mariadb"
check_template "$PG_TEMPLATE" "$PG_IP" "postgresql"

echo ""
echo "[4/4] Resumen de comandos rápidos (ejecutar en PowerShell):"
echo ""
echo "  # Verificar VMs registradas:"
echo "  VBoxManage list vms"
echo ""
echo "  # Verificar snapshots:"
echo "  VBoxManage snapshot \"$MARIADB_TEMPLATE\" list"
echo "  VBoxManage snapshot \"$PG_TEMPLATE\" list"
echo ""
echo "  # Tomar snapshots (con VMs apagadas):"
echo "  VBoxManage snapshot \"$MARIADB_TEMPLATE\" take \"$SNAPSHOT_NAME\""
echo "  VBoxManage snapshot \"$PG_TEMPLATE\" take \"$SNAPSHOT_NAME\""
echo ""
echo "  # Probar SSH desde Windows:"
echo "  ssh -i \$env:USERPROFILE\\.ssh\\id_rsa root@$MARIADB_IP"
echo "  ssh -i \$env:USERPROFILE\\.ssh\\id_rsa root@$PG_IP"
echo ""
echo "=================================================="
echo "  Cuando ambas plantillas estén listas, ejecuta:"
echo "  go run ./cmd/server"
echo "  y abre http://localhost:8080"
echo "=================================================="