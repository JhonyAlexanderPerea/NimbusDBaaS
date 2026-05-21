package provisioner

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"nimbusDBaaS/internal/models"
	"nimbusDBaaS/internal/store"
)

type Config struct {
	MariaDBTemplate    string
	PostgreSQLTemplate string
	HostOnlyNet        string
	SSHKeyPath         string
	TemplateISOPath    string
	TemplateISOURL     string
	TemplateUser       string
	TemplatePassword   string
	TemplateSnapshot   string
	BaseIP             string
	VBoxManage         string
	Simulated          bool
}

func DefaultConfig() Config {
	sim := os.Getenv("NIMBUS_SIMULATED")
	simulated := sim == "" || sim == "1" || strings.ToLower(sim) == "true"
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return Config{
		MariaDBTemplate:    getEnv("NIMBUS_MARIADB_TEMPLATE", "nimbus-mariadb-template"),
		PostgreSQLTemplate: getEnv("NIMBUS_PG_TEMPLATE", "nimbus-pg-template"),
		HostOnlyNet:        getEnv("NIMBUS_HOST_ONLY_NET", "vboxnet0"),
		SSHKeyPath:         getEnv("NIMBUS_SSH_KEY", filepath.Join(home, ".ssh", "nimbus_id_rsa")),
		TemplateISOPath:    getEnv("NIMBUS_TEMPLATE_ISO", filepath.Join(home, "Downloads", "debian-13.4.0-amd64-netinst.iso")),
		TemplateISOURL:     getEnv("NIMBUS_TEMPLATE_ISO_URL", "https://cdimage.debian.org/debian-cd/current/amd64/iso-cd/"),
		TemplateUser:       getEnv("NIMBUS_TEMPLATE_USER", "nimbus"),
		TemplatePassword:   getEnv("NIMBUS_TEMPLATE_PASSWORD", "nimbus-vm"),
		TemplateSnapshot:   getEnv("NIMBUS_TEMPLATE_SNAPSHOT", "base"),
		BaseIP:             getEnv("NIMBUS_BASE_IP", "192.168.56"),
		VBoxManage:         getEnv("VBOXMANAGE", "VBoxManage"),
		Simulated:          simulated,
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type Provisioner struct {
	cfg   Config
	store *store.Store
}

func New(cfg Config, s *store.Store) *Provisioner {
	p := &Provisioner{cfg: cfg, store: s}
	p.StartHealthCheck(10 * time.Second)
	return p
}

func (p *Provisioner) Provision(inst *models.Instance, sqlContent string) {
	go func() {
		if err := p.provision(inst, sqlContent); err != nil {
			log.Printf("[provisioner] ERROR instance %s: %v", inst.ID, err)
			inst.Status = models.StatusError
			inst.ErrorMsg = err.Error()
			_ = p.store.UpdateInstance(inst)
			_ = p.store.AddLog("ERROR", fmt.Sprintf("Error provisionando %s: %v", inst.DBName, err), inst.ID)
		}
	}()
}

func (p *Provisioner) provision(inst *models.Instance, sqlContent string) error {
	_ = p.store.AddLog("INFO",
		fmt.Sprintf("Solicitud de creación de la base de datos %s con usuario %s en %s",
			inst.DBName, inst.Username, engineLabel(inst.Engine)), inst.ID)

	inst.VMName = fmt.Sprintf("nimbus-%s-%s", inst.Engine, inst.ID[:8])

	if p.cfg.Simulated {
		return p.simulatedProvision(inst, sqlContent)
	}
	return p.realProvision(inst, sqlContent)
}

func (p *Provisioner) simulatedProvision(inst *models.Instance, sqlContent string) error {
	type step struct {
		d time.Duration
		f func() error
	}
	steps := []step{
		{600 * time.Millisecond, func() error {
			return p.store.AddLog("INFO",
				fmt.Sprintf("Clonando plantilla %s para la instancia %s", engineLabel(inst.Engine), inst.VMName), inst.ID)
		}},
		{900 * time.Millisecond, func() error {
			return p.store.AddLog("INFO",
				fmt.Sprintf("Configurando red host-only para %s", inst.VMName), inst.ID)
		}},
		{1000 * time.Millisecond, func() error {
			return p.store.AddLog("INFO",
				fmt.Sprintf("Iniciando máquina virtual %s", inst.VMName), inst.ID)
		}},
		{1500 * time.Millisecond, func() error {
			inst.Host = simulatedIP(p.cfg.BaseIP)
			inst.Port = defaultPort(inst.Engine)
			inst.AccessCmd = accessCmd(inst)
			_ = p.store.UpdateInstance(inst)
			return p.store.AddLog("OK",
				fmt.Sprintf("Creación de la MV con %s para la ejecución de la base de datos %s",
					engineLabel(inst.Engine), inst.DBName), inst.ID)
		}},
		{800 * time.Millisecond, func() error {
			return p.store.AddLog("INFO",
				fmt.Sprintf("Conectando por SSH a %s para crear la base de datos %s", inst.Host, inst.DBName), inst.ID)
		}},
		{600 * time.Millisecond, func() error {
			return p.store.AddLog("INFO",
				fmt.Sprintf("Creando usuario %s y asignando privilegios en %s", inst.Username, inst.DBName), inst.ID)
		}},
	}

	if sqlContent != "" {
		steps = append(steps, step{400 * time.Millisecond, func() error {
			return p.store.AddLog("INFO",
				fmt.Sprintf("Ejecutando archivo SQL en la base de datos %s", inst.DBName), inst.ID)
		}})
	}

	for _, s := range steps {
		time.Sleep(s.d)
		if err := s.f(); err != nil {
			return err
		}
	}

	inst.Status = models.StatusRunning
	if err := p.store.UpdateInstance(inst); err != nil {
		return err
	}
	return p.store.AddLog("OK",
		fmt.Sprintf("Instancia %s lista — host asignado %s", inst.DBName, inst.Host), inst.ID)
}

func (p *Provisioner) realProvision(inst *models.Instance, sqlContent string) error {
	template := p.cfg.MariaDBTemplate
	if inst.Engine == models.EnginePostgreSQL {
		template = p.cfg.PostgreSQLTemplate
	}

	if err := p.ensureTemplateReady(template, inst.Engine); err != nil {
		return fmt.Errorf("preparar plantilla: %w", err)
	}

	_ = p.store.AddLog("INFO", fmt.Sprintf("Clonando plantilla %s → %s (disco multiconexión)", template, inst.VMName), inst.ID)
	if err := p.vbm("clonevm", template, "--snapshot", p.cfg.TemplateSnapshot, "--options", "link", "--name", inst.VMName, "--register"); err != nil {
		return fmt.Errorf("clonar VM: %w", err)
	}
	if err := p.vbm("modifyvm", inst.VMName, "--nic1", "hostonly", "--hostonlyadapter1", p.cfg.HostOnlyNet); err != nil {
		return fmt.Errorf("configurar red: %w", err)
	}
	_ = p.store.AddLog("INFO", fmt.Sprintf("Iniciando VM %s", inst.VMName), inst.ID)
	if err := p.vbm("startvm", inst.VMName, "--type", "headless"); err != nil {
		return fmt.Errorf("iniciar VM: %w", err)
	}

	ip, err := p.waitForIP(inst.VMName, 90*time.Second)
	if err != nil {
		return fmt.Errorf("esperar IP: %w", err)
	}
	inst.Host = ip
	inst.Port = defaultPort(inst.Engine)
	inst.AccessCmd = accessCmd(inst)
	_ = p.store.AddLog("OK",
		fmt.Sprintf("Creación de la MV con %s para la ejecución de la base de datos %s", engineLabel(inst.Engine), inst.DBName), inst.ID)
	_ = p.store.UpdateInstance(inst)

	_ = p.store.AddLog("INFO", fmt.Sprintf("Conectando por SSH a %s", ip), inst.ID)
	if err := p.sshProvision(inst, sqlContent); err != nil {
		return fmt.Errorf("provisionar DB: %w", err)
	}

	inst.Status = models.StatusRunning
	_ = p.store.UpdateInstance(inst)
	return p.store.AddLog("OK", fmt.Sprintf("Instancia %s lista — host asignado %s", inst.DBName, inst.Host), inst.ID)
}

func (p *Provisioner) sshProvision(inst *models.Instance, sqlContent string) error {
	sshArgs := []string{
		"-i", p.cfg.SSHKeyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "ConnectTimeout=10",
		fmt.Sprintf("root@%s", inst.Host),
	}

	run := func(cmd string) error {
		args := append(sshArgs, cmd)
		out, err := exec.Command("ssh", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("ssh %q: %w\n%s", cmd, err, out)
		}
		return nil
	}

	var cmds []string
	if inst.Engine == models.EngineMariaDB {
		cmds = []string{
			fmt.Sprintf("mariadb -u root -e \"CREATE DATABASE IF NOT EXISTS \\`%s\\`;\"", inst.DBName),
			fmt.Sprintf("mariadb -u root -e \"CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s';\"", inst.Username, inst.Password),
			fmt.Sprintf("mariadb -u root -e \"GRANT ALL PRIVILEGES ON \\`%s\\`.* TO '%s'@'%%'; FLUSH PRIVILEGES;\"", inst.DBName, inst.Username),
		}
	} else {
		cmds = []string{
			fmt.Sprintf("sudo -u postgres psql -c \"CREATE DATABASE %s;\"", inst.DBName),
			fmt.Sprintf("sudo -u postgres psql -c \"CREATE USER %s WITH PASSWORD '%s';\"", inst.Username, inst.Password),
			fmt.Sprintf("sudo -u postgres psql -c \"GRANT ALL PRIVILEGES ON DATABASE %s TO %s;\"", inst.DBName, inst.Username),
		}
	}
	for _, c := range cmds {
		if err := run(c); err != nil {
			return err
		}
	}

	if sqlContent != "" {
		_ = p.store.AddLog("INFO", fmt.Sprintf("Ejecutando archivo SQL en %s", inst.DBName), inst.ID)
		tmp := fmt.Sprintf("/tmp/nimbus_%s.sql", inst.ID[:8])
		if err := run(fmt.Sprintf("cat > %s << 'NEOF'\n%s\nNEOF", tmp, sqlContent)); err != nil {
			return err
		}
		var execCmd string
		if inst.Engine == models.EngineMariaDB {
			execCmd = fmt.Sprintf("mariadb -u %s -p%s %s < %s", inst.Username, inst.Password, inst.DBName, tmp)
		} else {
			execCmd = fmt.Sprintf("PGPASSWORD=%s psql -U %s -d %s -f %s", inst.Password, inst.Username, inst.DBName, tmp)
		}
		if err := run(execCmd); err != nil {
			return err
		}
		_ = run("rm " + tmp)
	}
	return nil
}

func (p *Provisioner) Destroy(inst *models.Instance) error {
	_ = p.store.AddLog("INFO", fmt.Sprintf("Eliminando instancia %s", inst.DBName), inst.ID)
	if p.cfg.Simulated {
		time.Sleep(300 * time.Millisecond)
		_ = p.store.DeleteInstance(inst.ID)
		return p.store.AddLog("OK", fmt.Sprintf("Instancia %s de base de datos eliminada", inst.DBName), inst.ID)
	}
	if err := p.CleanupVM(inst); err != nil {
		_ = p.store.AddLog("WARN", fmt.Sprintf("No se pudo eliminar la VM %s: %v", inst.VMName, err), inst.ID)
	}
	_ = p.store.DeleteInstance(inst.ID)
	return p.store.AddLog("OK", fmt.Sprintf("Instancia %s de base de datos eliminada", inst.DBName), inst.ID)
}

func (p *Provisioner) CleanupVM(inst *models.Instance) error {
	if p.cfg.Simulated || inst.VMName == "" {
		return nil
	}
	registered, err := p.vmExists(inst.VMName)
	if err != nil {
		return err
	}
	if !registered {
		return nil
	}
	_ = p.vbm("controlvm", inst.VMName, "poweroff")
	time.Sleep(2 * time.Second)
	if err := p.vbm("unregistervm", inst.VMName, "--delete"); err != nil {
		return err
	}
	return nil
}

func (p *Provisioner) vbm(args ...string) error {
	cmd := exec.Command(p.cfg.VBoxManage, args...)
	var stdout, buf bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		msg := buf.String()
		if msg == "" {
			msg = stdout.String()
		}
		if msg == "" {
			msg = strings.Join(args, " ")
		}
		return fmt.Errorf("%w: %s", err, msg)
	}
	return nil
}

func (p *Provisioner) vbmOutput(args ...string) (string, error) {
	cmd := exec.Command(p.cfg.VBoxManage, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if msg == "" {
			msg = stdout.String()
		}
		if msg == "" {
			msg = strings.Join(args, " ")
		}
		return "", fmt.Errorf("%w: %s", err, msg)
	}
	return stdout.String(), nil
}

func (p *Provisioner) ensureTemplateReady(templateName string, engine models.Engine) error {
	registered, err := p.vmExists(templateName)
	if err != nil {
		return err
	}
	if registered {
		return p.ensureTemplateSnapshot(templateName)
	}
	return p.bootstrapTemplate(templateName, engine)
}

func (p *Provisioner) ensureTemplateSnapshot(templateName string) error {
	hasSnapshot, err := p.snapshotExists(templateName, p.cfg.TemplateSnapshot)
	if err != nil {
		return err
	}
	if hasSnapshot {
		return p.ensureTemplateDiskMultiattach(templateName)
	}
	_ = p.store.AddLog("INFO", fmt.Sprintf("La plantilla %s existe pero no tiene snapshot %s; NimbusDBaaS lo creará", templateName, p.cfg.TemplateSnapshot), "")
	if err := p.vbm("snapshot", templateName, "take", p.cfg.TemplateSnapshot, "--description", "NimbusDBaaS base template"); err != nil {
		return fmt.Errorf("crear snapshot base: %w", err)
	}
	return p.ensureTemplateDiskMultiattach(templateName)
}

func (p *Provisioner) snapshotExists(vmName, snapshotName string) (bool, error) {
	out, err := p.vbmOutput("snapshot", vmName, "list", "--machinereadable")
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "does not have any snapshots") || strings.Contains(msg, "vbox_e_object_not_found") {
			return false, nil
		}
		return false, err
	}
	needle := fmt.Sprintf(`SnapshotName="%s"`, snapshotName)
	if strings.Contains(out, needle) {
		return true, nil
	}
	return false, nil
}

func (p *Provisioner) vmExists(vmName string) (bool, error) {
	out, err := p.vbmOutput("list", "vms")
	if err != nil {
		return false, err
	}
	return strings.Contains(out, fmt.Sprintf("\"%s\"", vmName)), nil
}

func (p *Provisioner) bootstrapTemplate(templateName string, engine models.Engine) error {
	_ = p.store.AddLog("INFO", fmt.Sprintf("La plantilla %s no existe; NimbusDBaaS la creará automáticamente", templateName), "")

	hostOnlyNet, err := p.ensureHostOnlyNet()
	if err != nil {
		return err
	}
	if err := p.ensureSSHKeyPair(); err != nil {
		return err
	}
	if err := p.ensureDebianISO(); err != nil {
		return err
	}

	registered, err := p.vmExists(templateName)
	if err != nil {
		return err
	}
	if !registered {
		if err := p.vbm("createvm", "--name", templateName, "--ostype", "Debian_64", "--register"); err != nil {
			return fmt.Errorf("crear plantilla: %w", err)
		}
	}

	if err := p.vbm("modifyvm", templateName,
		"--memory", "1024",
		"--cpus", "1",
		"--nic1", "hostonly",
		"--hostonlyadapter1", hostOnlyNet,
		"--audio", "none",
		"--usb", "off",
		"--boot1", "dvd",
		"--boot2", "disk"); err != nil {
		return fmt.Errorf("configurar plantilla: %w", err)
	}

	diskPath := p.templateDiskPath(templateName)
	if err := p.vbm("createmedium", "disk", "--filename", diskPath, "--size", "8192", "--format", "VDI"); err != nil {
		return fmt.Errorf("crear disco base: %w", err)
	}
	if err := p.vbm("storagectl", templateName, "--name", "SATA", "--add", "sata", "--controller", "IntelAhci"); err != nil {
		return fmt.Errorf("crear controlador SATA: %w", err)
	}
	if err := p.vbm("storageattach", templateName, "--storagectl", "SATA", "--port", "0", "--device", "0", "--type", "hdd", "--medium", diskPath); err != nil {
		return fmt.Errorf("adjuntar disco base: %w", err)
	}
	if err := p.vbm("storagectl", templateName, "--name", "IDE", "--add", "ide"); err != nil {
		return fmt.Errorf("crear controlador IDE: %w", err)
	}
	if err := p.vbm("storageattach", templateName, "--storagectl", "IDE", "--port", "0", "--device", "0", "--type", "dvddrive", "--medium", p.cfg.TemplateISOPath); err != nil {
		return fmt.Errorf("adjuntar ISO Debian: %w", err)
	}

	if err := p.vbm("unattended", "install", templateName,
		"--iso", p.cfg.TemplateISOPath,
		"--user", p.cfg.TemplateUser,
		"--password", p.cfg.TemplatePassword,
		"--full-user-name", "NimbusDBaaS",
		"--install-additions",
		"--locale", "en_US",
		"--country", "US",
		"--time-zone", "UTC",
		"--hostname", templateName+".localdomain",
		"--package-selection-adjustment", "minimal",
		"--start-vm", "headless"); err != nil {
		return fmt.Errorf("instalacion unattended: %w", err)
	}

	if err := p.waitForGuestAdditions(templateName, 20*time.Minute); err != nil {
		return fmt.Errorf("esperar guest additions: %w", err)
	}

	if err := p.provisionTemplateGuest(templateName, engine); err != nil {
		return err
	}

	if err := p.vbm("controlvm", templateName, "poweroff"); err != nil {
		return fmt.Errorf("apagar plantilla: %w", err)
	}
	if err := p.vbm("snapshot", templateName, "take", p.cfg.TemplateSnapshot, "--description", "NimbusDBaaS base template"); err != nil {
		return fmt.Errorf("crear snapshot base: %w", err)
	}
	return p.ensureTemplateDiskMultiattach(templateName)
}

func (p *Provisioner) ensureTemplateDiskMultiattach(templateName string) error {
	diskPath, err := p.templateMediumPath(templateName)
	if err != nil {
		return err
	}
	if err := p.vbm("modifymedium", "disk", diskPath, "--type", "multiattach"); err != nil {
		return fmt.Errorf("configurar disco multiattach: %w", err)
	}
	return nil
}

func (p *Provisioner) templateMediumPath(templateName string) (string, error) {
	out, err := p.vbmOutput("showvminfo", templateName, "--machinereadable")
	if err != nil {
		return "", err
	}
	re := regexp.MustCompile(`(?m)^[A-Za-z0-9_-]+-0-0="([^"]+)"$`)
	matches := re.FindAllStringSubmatch(out, -1)
	for _, match := range matches {
		if len(match) >= 2 && strings.HasSuffix(strings.ToLower(match[1]), ".vdi") {
			return match[1], nil
		}
	}
	for _, match := range matches {
		if len(match) >= 2 && match[1] != "" {
			return match[1], nil
		}
	}
	return "", fmt.Errorf("no se pudo determinar el disco de la plantilla %s", templateName)
}

func (p *Provisioner) provisionTemplateGuest(templateName string, engine models.Engine) error {
	pubPath := p.cfg.SSHKeyPath + ".pub"
	keyData, err := os.ReadFile(pubPath)
	if err != nil {
		return fmt.Errorf("leer llave publica SSH: %w", err)
	}

	keyFile, err := os.CreateTemp("", "nimbus-authorized_keys-*.pub")
	if err != nil {
		return err
	}
	if _, err := keyFile.Write(keyData); err != nil {
		_ = keyFile.Close()
		_ = os.Remove(keyFile.Name())
		return err
	}
	if err := keyFile.Close(); err != nil {
		_ = os.Remove(keyFile.Name())
		return err
	}
	defer os.Remove(keyFile.Name())

	if err := p.guestRun(templateName, "root", p.cfg.TemplatePassword, `mkdir -p /root/.ssh && chmod 700 /root/.ssh`); err != nil {
		return fmt.Errorf("preparar ssh root: %w", err)
	}
	if err := p.guestCopyTo(templateName, "root", p.cfg.TemplatePassword, keyFile.Name(), "/root/.ssh/"); err != nil {
		return fmt.Errorf("copiar llave SSH: %w", err)
	}
	if err := p.guestRun(templateName, "root", p.cfg.TemplatePassword, `set -e; mv /root/.ssh/`+filepath.Base(keyFile.Name())+` /root/.ssh/authorized_keys; chmod 600 /root/.ssh/authorized_keys`); err != nil {
		return fmt.Errorf("activar llave SSH: %w", err)
	}

	if engine == models.EngineMariaDB {
		script := `set -e
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y openssh-server mariadb-server
sed -i 's/^#\?bind-address.*/bind-address = 0.0.0.0/' /etc/mysql/mariadb.conf.d/50-server.cnf
systemctl enable ssh mariadb`
		if err := p.guestRun(templateName, "root", p.cfg.TemplatePassword, script); err != nil {
			return fmt.Errorf("configurar plantilla MariaDB: %w", err)
		}
		return nil
	}

	script := `set -e
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y openssh-server postgresql
sed -i "s/^#\?listen_addresses =.*/listen_addresses = '*'" /etc/postgresql/*/main/postgresql.conf
grep -q '^host all all 0.0.0.0/0' /etc/postgresql/*/main/pg_hba.conf || echo 'host all all 0.0.0.0/0 md5' >> /etc/postgresql/*/main/pg_hba.conf
systemctl enable ssh postgresql`
	if err := p.guestRun(templateName, "root", p.cfg.TemplatePassword, script); err != nil {
		return fmt.Errorf("configurar plantilla PostgreSQL: %w", err)
	}
	return nil
}

func (p *Provisioner) waitForGuestAdditions(vmName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if out, err := p.vbmOutput("guestproperty", "get", vmName, "/VirtualBox/GuestAdd/Version"); err == nil && strings.Contains(out, "Value:") {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("timeout esperando guest additions de %s", vmName)
}

func (p *Provisioner) guestRun(vmName, username, password, shellCommand string) error {
	args := []string{"guestcontrol", vmName, "run", "--username", username, "--password", password, "--wait-stdout", "--wait-stderr", "--exe", "/bin/sh", "--", "-lc", shellCommand}
	return p.vbm(args...)
}

func (p *Provisioner) guestCopyTo(vmName, username, password, hostSource, guestTarget string) error {
	args := []string{"guestcontrol", vmName, "copyto", "--username", username, "--password", password, "--recursive", "--target-directory", guestTarget, hostSource}
	return p.vbm(args...)
}

func (p *Provisioner) templateDiskPath(templateName string) string {
	baseDir := filepath.Dir(p.cfg.SSHKeyPath)
	if baseDir == "." {
		if home, err := os.UserHomeDir(); err == nil {
			baseDir = home
		}
	}
	return filepath.Join(baseDir, templateName+".vdi")
}

func (p *Provisioner) ensureSSHKeyPair() error {
	if _, err := os.Stat(p.cfg.SSHKeyPath); err == nil {
		if _, err := os.Stat(p.cfg.SSHKeyPath + ".pub"); err == nil {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(p.cfg.SSHKeyPath), 0700); err != nil {
		return err
	}
	cmd := exec.Command("ssh-keygen", "-t", "rsa", "-b", "4096", "-f", p.cfg.SSHKeyPath, "-N", "", "-C", "nimbus-dbaas")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("generar llave SSH: %w: %s", err, buf.String())
	}
	return nil
}

func (p *Provisioner) ensureDebianISO() error {
	if ok, err := isValidISOFile(p.cfg.TemplateISOPath); err != nil {
		return err
	} else if ok {
		return nil
	} else if _, err := os.Stat(p.cfg.TemplateISOPath); err == nil {
		_ = os.Remove(p.cfg.TemplateISOPath)
	}
	if err := os.MkdirAll(filepath.Dir(p.cfg.TemplateISOPath), 0755); err != nil {
		return err
	}
	isoURL, err := p.resolveDebianISOURL()
	if err != nil {
		return err
	}
	resp, err := http.Get(isoURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("descarga ISO: %s", resp.Status)
	}
	file, err := os.Create(p.cfg.TemplateISOPath)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := io.Copy(file, resp.Body); err != nil {
		return err
	}
	return nil
}

func isValidISOFile(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if info.Size() < 50*1024*1024 {
		return false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	if _, err := file.Seek(32769, io.SeekStart); err != nil {
		return false, nil
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(file, buf); err != nil {
		return false, nil
	}
	return string(buf) == "CD001", nil
}

func (p *Provisioner) resolveDebianISOURL() (string, error) {
	if ok, err := p.urlReachable(p.cfg.TemplateISOURL); err == nil && ok {
		return p.cfg.TemplateISOURL, nil
	}

	indexURL := p.cfg.TemplateISOURL
	if strings.HasSuffix(indexURL, ".iso") {
		if idx := strings.LastIndex(indexURL, "/"); idx >= 0 {
			indexURL = indexURL[:idx+1]
		}
	} else if !strings.HasSuffix(indexURL, "/") {
		indexURL += "/"
	}

	resp, err := http.Get(indexURL)
	if err != nil {
		return "", fmt.Errorf("resolver índice Debian: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("resolver índice Debian: %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("resolver índice Debian: %w", err)
	}
	re := regexp.MustCompile(`debian-[0-9.]+-amd64-netinst\.iso`)
	match := re.FindString(string(body))
	if match == "" {
		return "", fmt.Errorf("resolver índice Debian: no se encontró netinst amd64")
	}
	return indexURL + match, nil
}

func (p *Provisioner) urlReachable(url string) (bool, error) {
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300, nil
}

func (p *Provisioner) ensureHostOnlyNet() (string, error) {
	out, err := p.vbmOutput("list", "hostonlyifs")
	if err != nil {
		return "", err
	}
	if strings.Contains(out, p.cfg.HostOnlyNet) {
		return p.cfg.HostOnlyNet, nil
	}
	created, err := p.vbmOutput("hostonlyif", "create")
	if err != nil {
		return "", fmt.Errorf("crear host-only: %w", err)
	}
	createdName := p.cfg.HostOnlyNet
	if start := strings.Index(created, "'"); start >= 0 {
		if end := strings.Index(created[start+1:], "'"); end >= 0 {
			createdName = created[start+1 : start+1+end]
		}
	}
	if err := p.vbm("hostonlyif", "ipconfig", createdName, "--ip", "192.168.56.1", "--netmask", "255.255.255.0"); err != nil {
		return "", fmt.Errorf("configurar host-only: %w", err)
	}
	return createdName, nil
}

func (p *Provisioner) waitForIP(vmName string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command(p.cfg.VBoxManage, "guestproperty", "get", vmName,
			"/VirtualBox/GuestInfo/Net/0/V4/IP").Output()
		if err == nil {
			line := strings.TrimSpace(string(out))
			if strings.HasPrefix(line, "Value:") {
				ip := strings.TrimSpace(strings.TrimPrefix(line, "Value:"))
				if ip != "" && ip != "0.0.0.0" {
					return ip, nil
				}
			}
		}
		time.Sleep(3 * time.Second)
	}
	return "", fmt.Errorf("timeout esperando IP de %s", vmName)
}

func simulatedIP(base string) string {
	n, _ := rand.Int(rand.Reader, big.NewInt(200))
	return fmt.Sprintf("%s.%d", base, n.Int64()+20)
}

func defaultPort(e models.Engine) int {
	if e == models.EngineMariaDB {
		return 3306
	}
	return 5432
}

func accessCmd(inst *models.Instance) string {
	if inst.Engine == models.EngineMariaDB {
		return fmt.Sprintf("mariadb -h %s -u %s -p%s %s", inst.Host, inst.Username, inst.Password, inst.DBName)
	}
	return fmt.Sprintf("psql -h %s -U %s -d %s", inst.Host, inst.Username, inst.DBName)
}

func engineLabel(e models.Engine) string {
	if e == models.EngineMariaDB {
		return "MariaDB"
	}
	return "PostgreSQL"
}

func GeneratePassword() string {
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:12]
}

// StartHealthCheck inicia la verificación periódica de las máquinas virtuales.
func (p *Provisioner) StartHealthCheck(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		for range ticker.C {
			p.checkInstancesHealth()
		}
	}()
}

// checkInstancesHealth revisa si las VMs de las instancias siguen corriendo.
func (p *Provisioner) checkInstancesHealth() {
	instances, err := p.store.ListInstances()
	if err != nil {
		return
	}
	for _, inst := range instances {
		if inst.Status == models.StatusRunning {
			if p.cfg.Simulated {
				continue
			}
			running, err := p.isVMRunning(inst.VMName)
			if err != nil {
				log.Printf("[health-check] Error verificando estado de VM %s: %v", inst.VMName, err)
				continue
			}
			if !running {
				log.Printf("[health-check] La VM %s está apagada. Cambiando estado a 'apagada'.", inst.VMName)
				inst.Status = models.StatusStopped
				_ = p.store.UpdateInstance(inst)
				_ = p.store.AddLog("WARN", fmt.Sprintf("La máquina virtual de la instancia %s se encuentra apagada o detenida", inst.DBName), inst.ID)
			}
		} else if inst.Status == models.StatusStopped {
			if p.cfg.Simulated {
				continue
			}
			running, err := p.isVMRunning(inst.VMName)
			if err == nil && running {
				log.Printf("[health-check] La VM %s volvió a ejecutarse. Cambiando estado a 'running'.", inst.VMName)
				inst.Status = models.StatusRunning
				_ = p.store.UpdateInstance(inst)
				_ = p.store.AddLog("OK", fmt.Sprintf("La máquina virtual de la instancia %s se ha iniciado nuevamente", inst.DBName), inst.ID)
			}
		}
	}
}

// isVMRunning verifica si la máquina virtual está corriendo en VirtualBox.
func (p *Provisioner) isVMRunning(vmName string) (bool, error) {
	cmd := exec.Command(p.cfg.VBoxManage, "showvminfo", vmName, "--machinereadable")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errStr := stderr.String()
		outStr := stdout.String()
		if strings.Contains(errStr, "could not find a registered virtual machine") ||
			strings.Contains(outStr, "could not find a registered virtual machine") {
			return false, nil
		}
		return false, fmt.Errorf("%w: %s", err, errStr)
	}

	lines := strings.Split(stdout.String(), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "VMState=") {
			state := strings.Trim(strings.TrimPrefix(line, "VMState="), "\"\r\n")
			return state == "running", nil
		}
	}
	return false, nil
}
