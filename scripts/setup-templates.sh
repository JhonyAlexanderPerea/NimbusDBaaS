#!/usr/bin/env bash
# =============================================================================
# NimbusDBaaS – Setup de plantillas VirtualBox
# =============================================================================
# Este script documenta los pasos para crear las VMs plantilla de MariaDB y
# PostgreSQL que el servicio usará como base para clonar nuevas instancias.
#
# Ejecutar una sola vez en el host donde corra NimbusDBaaS.
# =============================================================================
set -euo pipefail

VM_NET="vboxnet0"
SSH_KEY="$HOME/.ssh/id_rsa"
TEMPLATE_USER="${NIMBUS_TEMPLATE_USER:-nimbus}"
ISO_INDEX_URL="https://cdimage.debian.org/debian-cd/current/amd64/iso-cd/"

echo "=== NimbusDBaaS: Setup de plantillas VirtualBox ==="

# ── 0. Generar par de llaves SSH ───────────────────────────────────────────
if [ ! -f "$SSH_KEY" ]; then
  echo "[1/7] Generando par de llaves SSH para el servicio…"
  mkdir -p "$HOME/.ssh"
  ssh-keygen -t rsa -b 4096 -f "$SSH_KEY" -N "" -C "nimbus-dbaas"
  echo "      Llave pública: ${SSH_KEY}.pub"
else
  echo "[1/7] Llave SSH ya existe: $SSH_KEY"
fi

# ── 1. Crear adaptador host-only ───────────────────────────────────────────
echo "[2/7] Verificando adaptador host-only '$VM_NET'…"
if ! VBoxManage list hostonlyifs | grep -q "Name:.*$VM_NET"; then
  VBoxManage hostonlyif create
  VBoxManage hostonlyif ipconfig "$VM_NET" --ip 192.168.56.1 --netmask 255.255.255.0
  echo "      Adaptador $VM_NET creado."
else
  echo "      Adaptador $VM_NET ya existe."
fi

# ── 2. Descargar ISO Debian ────────────────────────────────────────────────
ISO_PATH="$HOME/Downloads/debian-13.4.0-amd64-netinst.iso"
if [ ! -f "$ISO_PATH" ]; then
  echo "[3/7] Resolviendo ISO Debian actual…"
  mkdir -p "$HOME/Downloads"
  ISO_FILE=$(curl -fsSL "$ISO_INDEX_URL" | grep -oE 'debian-[0-9.]+-amd64-netinst\.iso' | head -n1)
  curl -L -o "$ISO_PATH" "${ISO_INDEX_URL}${ISO_FILE}"
else
  echo "[3/7] ISO Debian ya descargada: $ISO_PATH"
fi

# Obtener el directorio por defecto de las VMs de VirtualBox
if VBOX_FOLDER=$(VBoxManage list systemproperties | grep "Default machine folder:" | cut -d: -f2- | sed 's/^[ \t]*//;s/[ \t]*$//'); then
  # Reemplazar contrabarra por barra para bash
  VBOX_FOLDER="${VBOX_FOLDER//\\//}"
else
  VBOX_FOLDER="$HOME/VirtualBox VMs"
fi

# ── Helper: create_template <name> ────────────────────────────────────────
create_template() {
  local NAME="$1"
  local MEM=512  # MB

  echo ""
  echo "=== Creando plantilla: $NAME ==="

  # Crear VM
  VBoxManage createvm --name "$NAME" --ostype Debian_64 --register

  # Configurar hardware
  VBoxManage modifyvm "$NAME" \
    --memory "$MEM" --cpus 1 \
    --nic1 hostonly --hostonlyadapter1 "$VM_NET" \
    --audio none --usb off

  # Crear disco principal (8 GB)
  local DISK="${VBOX_FOLDER}/${NAME}/${NAME}.vdi"
  VBoxManage createmedium disk --filename "$DISK" --size 8192 --format VDI

  # Controlador SATA
  VBoxManage storagectl "$NAME" --name "SATA" --add sata --controller IntelAhci
  VBoxManage storageattach "$NAME" --storagectl "SATA" --port 0 --device 0 --type hdd --medium "$DISK"

  # ISO de instalación
  VBoxManage storagectl "$NAME" --name "IDE" --add ide
  VBoxManage storageattach "$NAME" --storagectl "IDE" --port 0 --device 0 --type dvddrive --medium "$ISO_PATH"

  echo ""
  echo ">>> Inicia la VM '$NAME' manualmente, instala Debian (CLI, sin escritorio),"
  echo "    configura SSH con la llave pública en /root/.ssh/authorized_keys:"
  echo ""
  cat "${SSH_KEY}.pub"
  echo ""
  echo "    Luego sigue con el script post-install correspondiente."
}

# ── 3. Crear plantillas ────────────────────────────────────────────────────
echo "[4/7] Creando plantilla MariaDB…"
create_template "nimbus-mariadb-template"

echo "[5/7] Creando plantilla PostgreSQL…"
create_template "nimbus-pg-template"

echo ""
echo "[6/7] =============================================================="
echo "  Pasos manuales requeridos después de instalar Debian en cada VM:"
echo "  =================================================================="
echo ""
echo "  Para nimbus-mariadb-template:"
echo "    apt update && apt install -y mariadb-server openssh-server"
echo "    mariadb -u root -e \"ALTER USER 'root'@'localhost' IDENTIFIED BY 'root'; FLUSH PRIVILEGES;\""
echo "    # Habilitar bind-address en /etc/mysql/mariadb.conf.d/50-server.cnf → 0.0.0.0"
echo "    systemctl enable mariadb ssh"
echo "    mkdir -p /root/.ssh && echo '$(cat ${SSH_KEY}.pub)' >> /root/.ssh/authorized_keys"
echo "    chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys"
echo "    # Instalar Guest Additions dentro de la VM"
echo "    apt install -y build-essential dkms linux-headers-\$(uname -r) virtualbox-guest-dkms virtualbox-guest-utils virtualbox-guest-x11"
echo "    systemctl enable vboxservice"
echo "    systemctl restart vboxservice"
echo ""
echo "  Para nimbus-pg-template:"
echo "    apt update && apt install -y postgresql openssh-server"
echo "    # Editar /etc/postgresql/*/main/pg_hba.conf → host all all 0.0.0.0/0 md5"
echo "    # Editar /etc/postgresql/*/main/postgresql.conf → listen_addresses = '*'"
echo "    systemctl enable postgresql ssh"
echo "    mkdir -p /root/.ssh && echo '$(cat ${SSH_KEY}.pub)' >> /root/.ssh/authorized_keys"
echo "    chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys"
echo "    # Instalar Guest Additions dentro de la VM"
echo "    apt install -y build-essential dkms linux-headers-\$(uname -r) virtualbox-guest-dkms virtualbox-guest-utils virtualbox-guest-x11"
echo "    systemctl enable vboxservice"
echo "    systemctl restart vboxservice"
echo ""
echo "[7/7] Cuando ambas VMs estén listas, apagar las VMs y configurar los discos en modo multiconexión:"
echo ""
echo "  1. Apagar las VMs manualmente o con los siguientes comandos:"
echo "    VBoxManage controlvm nimbus-mariadb-template poweroff"
echo "    VBoxManage controlvm nimbus-pg-template poweroff"
echo ""
echo "  2. Configurar los discos principales en modo multiconexión (multiattach):"
echo "    VBoxManage modifymedium disk \"${VBOX_FOLDER}/nimbus-mariadb-template/nimbus-mariadb-template.vdi\" --type multiattach"
echo "    VBoxManage modifymedium disk \"${VBOX_FOLDER}/nimbus-pg-template/nimbus-pg-template.vdi\" --type multiattach"
echo ""
echo "=== Setup completado. Ahora puedes iniciar NimbusDBaaS ==="
