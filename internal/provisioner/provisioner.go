package provisioner

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

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
}

func DefaultConfig() Config {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return Config{
		MariaDBTemplate:    getEnv("NIMBUS_MARIADB_TEMPLATE", "nimbus-mariadb-template"),
		PostgreSQLTemplate: getEnv("NIMBUS_PG_TEMPLATE", "nimbus-pg-template"),

		// Nombre del adaptador Host-Only en Windows — ajusta si el tuyo tiene número diferente
		HostOnlyNet: getEnv("NIMBUS_HOST_ONLY_NET", "VirtualBox Host-Only Ethernet Adapter"),

		// Usa tu llave RSA existente
		SSHKeyPath: getEnv("NIMBUS_SSH_KEY", filepath.Join(home, ".ssh", "id_rsa")),

		TemplateISOPath:  getEnv("NIMBUS_TEMPLATE_ISO", filepath.Join(home, "Downloads", "debian-13.4.0-amd64-netinst.iso")),
		TemplateISOURL:   getEnv("NIMBUS_TEMPLATE_ISO_URL", "https://cdimage.debian.org/debian-cd/current/amd64/iso-cd/"),
		TemplateUser:     getEnv("NIMBUS_TEMPLATE_USER", "root"),
		TemplatePassword: getEnv("NIMBUS_TEMPLATE_PASSWORD", "user"),
		TemplateSnapshot: getEnv("NIMBUS_TEMPLATE_SNAPSHOT", "base"),

		// Tu red Host-Only: 192.168.10.0/24
		BaseIP: getEnv("NIMBUS_BASE_IP", "192.168.10"),

		VBoxManage: getEnv("VBOXMANAGE", "VBoxManage"),
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

	inst.VMName = inst.DBName
	return p.realProvision(inst, sqlContent)
}

func (p *Provisioner) realProvision(inst *models.Instance, sqlContent string) error {
	template, err := p.ensureTemplateReady(inst.Engine)
	if err != nil {
		return fmt.Errorf("preparar plantilla: %w", err)
	}

	_ = p.store.AddLog("INFO", fmt.Sprintf("Clonando plantilla %s → %s (disco multiconexión)", template, inst.VMName), inst.ID)
	if err := p.vbm("clonevm", template,
		"--snapshot", p.cfg.TemplateSnapshot,
		"--options", "link",
		"--name", inst.VMName,
		"--register"); err != nil {
		return fmt.Errorf("clonar VM: %w", err)
	}

	_ = p.store.AddLog("INFO", fmt.Sprintf("Iniciando VM %s", inst.VMName), inst.ID)
	if err := p.vbm("startvm", inst.VMName, "--type", "headless"); err != nil {
		return fmt.Errorf("iniciar VM: %w", err)
	}

	// Asigna IP fija y espera a que SSH esté disponible
	ip, err := p.assignAndWaitForIP(inst)
	if err != nil {
		return fmt.Errorf("asignar IP: %w", err)
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

// assignAndWaitForIP calcula una IP libre en 192.168.10.20-254,
// arranca la VM con la IP de la plantilla y la reconfigura vía SSH.
func (p *Provisioner) assignAndWaitForIP(inst *models.Instance) (string, error) {
	ip, err := p.nextFreeIP()
	if err != nil {
		return "", err
	}
	// Primero intentamos la IP base que la plantilla usa por convención
	var bootIP string
	var tryBaseIP string
	if inst.Engine == models.EngineMariaDB {
		tryBaseIP = fmt.Sprintf("%s.11", p.cfg.BaseIP)
	} else {
		tryBaseIP = fmt.Sprintf("%s.10", p.cfg.BaseIP)
	}

	// Intentar SSH a la IP base (plantilla) — suele ser más rápido cuando guestprops no están disponibles
	if err := p.waitForSSH(tryBaseIP, 30*time.Second); err == nil {
		bootIP = tryBaseIP
		log.Printf("[provisioner] VM %s respondió en IP base %s → reasignando a %s", inst.VMName, bootIP, ip)
		_ = p.store.AddLog("INFO", fmt.Sprintf("SSH disponible en IP base %s", tryBaseIP), inst.ID)
	} else {
		// Fallback: intentar obtener la IP vía guestproperty
		bootIP, err = p.waitForGuestHostOnlyIP(inst.VMName, 90*time.Second)
		if err != nil {
			log.Printf("[provisioner] guestproperty no devolvió IP para %s: %v — probando escaneo SSH corto", inst.VMName, err)
			_ = p.store.AddLog("WARN", fmt.Sprintf("guestproperty no devolvió IP para %s: %v", inst.VMName, err), inst.ID)
			// Intentar un escaneo rápido en un conjunto reducido de IPs: tryBaseIP, la IP candidata, y .20-.30
			candidates := []string{tryBaseIP, ip}
			for i := 20; i <= 30; i++ {
				cand := fmt.Sprintf("%s.%d", p.cfg.BaseIP, i)
				if cand == ip || cand == tryBaseIP {
					continue
				}
				candidates = append(candidates, cand)
			}
			found := ""
			for _, c := range candidates {
				log.Printf("[provisioner] probando SSH en %s (escaneo corto)", c)
				_ = p.store.AddLog("INFO", fmt.Sprintf("Probando SSH en %s (escaneo corto)", c), inst.ID)
				if err := p.waitForSSH(c, 5*time.Second); err == nil {
					found = c
					log.Printf("[provisioner] encontrado SSH en %s", c)
					_ = p.store.AddLog("OK", fmt.Sprintf("Encontrado SSH en %s", c), inst.ID)
					break
				}
			}
			if found == "" {
				_ = p.store.AddLog("ERROR", fmt.Sprintf("No se encontró SSH en rango de escaneo para %s", inst.VMName), inst.ID)
				return "", fmt.Errorf("IP host-only no disponible en %s: %w", inst.VMName, err)
			}
			bootIP = found
		} else {
			log.Printf("[provisioner] VM %s arrancó con IP host-only %s → reasignando a %s", inst.VMName, bootIP, ip)
			_ = p.store.AddLog("INFO", fmt.Sprintf("Guestproperty devolvió IP %s para %s", bootIP, inst.VMName), inst.ID)
			// Aún así esperar a SSH en la IP detectada
			if err := p.waitForSSH(bootIP, 30*time.Second); err != nil {
				return "", fmt.Errorf("SSH no disponible en %s: %w", bootIP, err)
			}
		}
	}

	// Cambiar la IP estática en la VM clonada (ya entramos como root)
	changeIPCmd := fmt.Sprintf(
		"sed -i 's/%s/%s/g' /etc/network/interfaces && systemctl restart networking",
		bootIP, ip,
	)
	if err := p.runSSH(bootIP, changeIPCmd); err != nil {
		_ = p.store.AddLog("ERROR", fmt.Sprintf("Error cambiando IP en VM %s: %v", inst.VMName, err), inst.ID)
		return "", fmt.Errorf("cambiar IP en VM: %w", err)
	}
	_ = p.store.AddLog("INFO", fmt.Sprintf("IP cambiada en VM %s de %s a %s", inst.VMName, bootIP, ip), inst.ID)

	// Esperar a que SSH responda en la nueva IP
	if err := p.waitForSSH(ip, 30*time.Second); err != nil {
		return "", fmt.Errorf("SSH no disponible en nueva IP %s: %w", ip, err)
	}

	return ip, nil
}

func (p *Provisioner) waitForGuestHostOnlyIP(vmName string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for nic := 0; nic < 4; nic++ {
			path := fmt.Sprintf("/VirtualBox/GuestInfo/Net/%d/V4/IP", nic)
			if out, err := p.vbmOutput("guestproperty", "get", vmName, path); err == nil {
				line := strings.TrimSpace(out)
				if strings.HasPrefix(line, "Value:") {
					ip := strings.TrimSpace(strings.TrimPrefix(line, "Value:"))
					if ip != "" && ip != "0.0.0.0" && strings.HasPrefix(ip, p.cfg.BaseIP+".") {
						return ip, nil
					}
				}
			}
		}
		time.Sleep(3 * time.Second)
	}
	return "", fmt.Errorf("timeout esperando IP host-only de %s", vmName)
}

// nextFreeIP busca la primera IP libre en el rango .20 - .254
func (p *Provisioner) nextFreeIP() (string, error) {
	instances, err := p.store.ListInstances()
	if err != nil {
		return "", err
	}
	used := map[string]bool{}
	for _, inst := range instances {
		if inst.Host != "" {
			used[inst.Host] = true
		}
	}
	for i := 20; i <= 254; i++ {
		candidate := fmt.Sprintf("%s.%d", p.cfg.BaseIP, i)
		if !used[candidate] {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no hay IPs disponibles en el rango %s.20-%s.254", p.cfg.BaseIP, p.cfg.BaseIP)
}

// waitForSSH intenta conectar por SSH hasta que tenga éxito o se agote el tiempo
func (p *Provisioner) waitForSSH(ip string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		err := p.runSSH(ip, "echo ok")
		if err == nil {
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("timeout esperando SSH en %s", ip)
}

// runSSH ejecuta un comando remoto vía SSH
func (p *Provisioner) runSSH(ip, cmd string) error {
	user := p.cfg.TemplateUser
	addr := net.JoinHostPort(ip, "22")

	var authMethods []ssh.AuthMethod

	// Try private key if available
	if keyPath := p.cfg.SSHKeyPath; keyPath != "" {
		if keyBytes, err := os.ReadFile(keyPath); err == nil {
			if signer, err := ssh.ParsePrivateKey(keyBytes); err == nil {
				authMethods = append(authMethods, ssh.PublicKeys(signer))
			}
		}
	}

	// Fallback to template password if configured
	if p.cfg.TemplatePassword != "" {
		authMethods = append(authMethods, ssh.Password(p.cfg.TemplatePassword))
	}

	if len(authMethods) == 0 {
		return fmt.Errorf("no SSH auth methods available (no key and no template password)")
	}

	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	conn, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	defer conn.Close()

	sess, err := conn.NewSession()
	if err != nil {
		return fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()

	var bout bytes.Buffer
	var berr bytes.Buffer
	sess.Stdout = &bout
	sess.Stderr = &berr

	if err := sess.Run(cmd); err != nil {
		return fmt.Errorf("ssh run: %w: %s", err, berr.String())
	}
	return nil
}

func (p *Provisioner) sshProvision(inst *models.Instance, sqlContent string) error {
	run := func(cmd string) error {
		return p.runSSH(inst.Host, cmd)
	}

	var cmds []string
	if inst.Engine == models.EngineMariaDB {
		cmds = []string{
			fmt.Sprintf("mariadb -u root -e \"CREATE DATABASE IF NOT EXISTS `%s`;\"", inst.DBName),
			fmt.Sprintf("mariadb -u root -e \"CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s';\"", inst.Username, inst.Password),
			fmt.Sprintf("mariadb -u root -e \"GRANT ALL PRIVILEGES ON `%s`.* TO '%s'@'%%'; FLUSH PRIVILEGES;\"", inst.DBName, inst.Username),
		}
	} else {
		cmds = []string{
			fmt.Sprintf(`runuser -u postgres -- psql -c "CREATE DATABASE %s;"`, inst.DBName),
			fmt.Sprintf(`runuser -u postgres -- psql -c "CREATE USER %s WITH PASSWORD '%s';"`, inst.Username, inst.Password),
			fmt.Sprintf(`runuser -u postgres -- psql -c "GRANT ALL PRIVILEGES ON DATABASE %s TO %s;"`, inst.DBName, inst.Username),
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
	if err := p.CleanupVM(inst); err != nil {
		_ = p.store.AddLog("WARN", fmt.Sprintf("No se pudo eliminar la VM %s: %v", inst.VMName, err), inst.ID)
	}
	_ = p.store.DeleteInstance(inst.ID)
	return p.store.AddLog("OK", fmt.Sprintf("Instancia %s de base de datos eliminada", inst.DBName), inst.ID)
}

func (p *Provisioner) CleanupVM(inst *models.Instance) error {
	if inst.VMName == "" {
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
	return p.vbm("unregistervm", inst.VMName, "--delete")
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

func (p *Provisioner) ensureTemplateReady(engine models.Engine) (string, error) {
	templateName := p.templateForEngine(engine)
	registered, err := p.vmExists(templateName)
	if err != nil {
		return "", err
	}
	if !registered {
		return "", fmt.Errorf("la plantilla %s no existe en VirtualBox — créala manualmente según las instrucciones", templateName)
	}
	if err := p.ensureTemplateSnapshot(templateName); err != nil {
		return "", err
	}
	return templateName, nil
}

func (p *Provisioner) ensureTemplateSnapshot(templateName string) error {
	hasSnapshot, err := p.snapshotExists(templateName, p.cfg.TemplateSnapshot)
	if err != nil {
		return err
	}
	if !hasSnapshot {
		return fmt.Errorf(
			"la plantilla %s no tiene el snapshot '%s' — ejecútalo con: VBoxManage snapshot \"%s\" take \"%s\"",
			templateName, p.cfg.TemplateSnapshot, templateName, p.cfg.TemplateSnapshot,
		)
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
	return strings.Contains(out, fmt.Sprintf(`SnapshotName="%s"`, snapshotName)), nil
}

func (p *Provisioner) vmExists(vmName string) (bool, error) {
	out, err := p.vbmOutput("list", "vms")
	if err != nil {
		return false, err
	}
	return strings.Contains(out, fmt.Sprintf("\"%s\"", vmName)), nil
}

func (p *Provisioner) templateForEngine(engine models.Engine) string {
	if engine == models.EnginePostgreSQL {
		return p.cfg.PostgreSQLTemplate
	}
	return p.cfg.MariaDBTemplate
}

func (p *Provisioner) ensureTemplateDiskMultiattach(templateName string) error {
	diskPath, err := p.templateMediumPath(templateName)
	if err != nil {
		return err
	}
	_ = diskPath
	return nil
}

func (p *Provisioner) templateMediumPath(templateName string) (string, error) {
	out, err := p.vbmOutput("showvminfo", templateName, "--machinereadable")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, `="`) {
			continue
		}
		parts := strings.SplitN(line, `="`, 2)
		if len(parts) != 2 {
			continue
		}
		value := strings.TrimSuffix(parts[1], `"`)
		if value == "" {
			continue
		}
		lower := strings.ToLower(value)
		if strings.HasSuffix(lower, ".vdi") || strings.HasSuffix(lower, ".vmdk") || strings.HasSuffix(lower, ".vhd") || strings.HasSuffix(lower, ".hdd") {
			return value, nil
		}
	}
	return "", fmt.Errorf("no se pudo determinar el disco de la plantilla %s", templateName)
}

// ── Métodos de bootstrap automático (no se usan en tu flujo manual) ───────

func (p *Provisioner) ensureHostOnlyNet() (string, error) {
	out, err := p.vbmOutput("list", "hostonlyifs")
	if err != nil {
		return "", err
	}
	if strings.Contains(out, p.cfg.HostOnlyNet) {
		return p.cfg.HostOnlyNet, nil
	}
	return "", fmt.Errorf("red host-only '%s' no encontrada — créala en VirtualBox > Archivo > Administrador de red de anfitrión", p.cfg.HostOnlyNet)
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
	_, err = io.Copy(file, resp.Body)
	return err
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
	indexURL := p.cfg.TemplateISOURL
	if !strings.HasSuffix(indexURL, "/") {
		indexURL += "/"
	}
	resp, err := http.Get(indexURL)
	if err != nil {
		return "", fmt.Errorf("resolver índice Debian: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	re := regexp.MustCompile(`debian-[0-9.]+-amd64-netinst\.iso`)
	match := re.FindString(string(body))
	if match == "" {
		return "", fmt.Errorf("no se encontró netinst amd64 en %s", indexURL)
	}
	return indexURL + match, nil
}

// ── Health check ──────────────────────────────────────────────────────────

func (p *Provisioner) StartHealthCheck(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		for range ticker.C {
			p.checkInstancesHealth()
		}
	}()
}

func (p *Provisioner) checkInstancesHealth() {
	instances, err := p.store.ListInstances()
	if err != nil {
		return
	}
	for _, inst := range instances {
		if inst.Status == models.StatusRunning {
			running, err := p.isVMRunning(inst.VMName)
			if err != nil {
				log.Printf("[health-check] Error verificando estado de VM %s: %v", inst.VMName, err)
				continue
			}
			if !running {
				log.Printf("[health-check] La VM %s está apagada.", inst.VMName)
				inst.Status = models.StatusStopped
				_ = p.store.UpdateInstance(inst)
				_ = p.store.AddLog("WARN", fmt.Sprintf("La máquina virtual de la instancia %s se encuentra apagada o detenida", inst.DBName), inst.ID)
			}
		} else if inst.Status == models.StatusStopped {
			running, err := p.isVMRunning(inst.VMName)
			if err == nil && running {
				inst.Status = models.StatusRunning
				_ = p.store.UpdateInstance(inst)
				_ = p.store.AddLog("OK", fmt.Sprintf("La máquina virtual de la instancia %s se ha iniciado nuevamente", inst.DBName), inst.ID)
			}
		}
	}
}

func (p *Provisioner) isVMRunning(vmName string) (bool, error) {
	cmd := exec.Command(p.cfg.VBoxManage, "showvminfo", vmName, "--machinereadable")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errStr := stderr.String() + stdout.String()
		if strings.Contains(errStr, "could not find a registered virtual machine") {
			return false, nil
		}
		return false, fmt.Errorf("%w: %s", err, errStr)
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.HasPrefix(line, "VMState=") {
			state := strings.Trim(strings.TrimPrefix(line, "VMState="), "\"\r\n")
			return state == "running", nil
		}
	}
	return false, nil
}

// ── Helpers ───────────────────────────────────────────────────────────────

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
