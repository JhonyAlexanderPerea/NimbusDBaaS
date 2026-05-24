package provisioner

import (
	"bufio"
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
	"runtime"
	"strings"
	"sync"
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

		// Nombre del adaptador Host-Only en Windows
		HostOnlyNet: getEnv("NIMBUS_HOST_ONLY_NET", "VirtualBox Host-Only Ethernet Adapter"),

		// Llave RSA existente
		SSHKeyPath: getEnv("NIMBUS_SSH_KEY", filepath.Join(home, ".ssh", "id_rsa")),

		TemplateISOPath:  getEnv("NIMBUS_TEMPLATE_ISO", filepath.Join(home, "Downloads", "debian-13.4.0-amd64-netinst.iso")),
		TemplateISOURL:   getEnv("NIMBUS_TEMPLATE_ISO_URL", "https://cdimage.debian.org/debian-cd/current/amd64/iso-cd/"),
		TemplateSnapshot: getEnv("NIMBUS_TEMPLATE_SNAPSHOT", "base"),

		// SSH se conecta siempre como root usando la llave pública configurada en la plantilla
		TemplateUser: "root",

		// Red Host-Only: 192.168.10.0/24
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
	cfg         Config
	store       *store.Store
	ipMu        sync.Mutex
	reservedIPs map[string]time.Time
	healthMu    sync.Mutex
	downCount   map[string]int
}

const reservationTTL = 5 * time.Minute
const healthCheckFailureThreshold = 3

func New(cfg Config, s *store.Store) *Provisioner {
	p := &Provisioner{
		cfg:         cfg,
		store:       s,
		reservedIPs: make(map[string]time.Time),
		downCount:   make(map[string]int),
	}
	p.StartHealthCheck(10 * time.Second)
	go p.startReservationSweeper(time.Minute)
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
	if err := ValidateIdentifier(inst.DBName, "db_name", inst.Engine); err != nil {
		return err
	}
	if err := ValidateIdentifier(inst.Username, "username", inst.Engine); err != nil {
		return err
	}

	_ = p.store.AddLog("INFO",
		fmt.Sprintf("Solicitud de creación de la base de datos %s con usuario %s en %s",
			inst.DBName, inst.Username, engineLabel(inst.Engine)), inst.ID)

	// VMName único: motor + primeros 8 chars del UUID
	inst.VMName = fmt.Sprintf("nimbus-%s-%s", inst.Engine, inst.DBName)
	return p.realProvision(inst, sqlContent)
}

func (p *Provisioner) realProvision(inst *models.Instance, sqlContent string) (err error) {
	template, err := p.ensureTemplateReady(inst.Engine)
	if err != nil {
		return fmt.Errorf("preparar plantilla: %w", err)
	}

	reservedIP, err := p.reserveNextIP()
	if err != nil {
		return fmt.Errorf("reservar IP: %w", err)
	}
	_ = p.store.AddLog("INFO", fmt.Sprintf("Reserva temporal de IP %s para %s", reservedIP, inst.DBName), inst.ID)
	defer func() {
		if err == nil {
			if p.releaseReservedIP(reservedIP) {
				_ = p.store.AddLog("OK", fmt.Sprintf("Reserva de IP liberada: %s", reservedIP), inst.ID)
			}
			return
		}
		p.rollbackProvision(inst, reservedIP, err)
	}()

	_ = p.store.AddLog("INFO", fmt.Sprintf("Clonando plantilla %s → %s (linked clone)", template, inst.VMName), inst.ID)
	if err = p.vbm("clonevm", template,
		"--snapshot", p.cfg.TemplateSnapshot,
		"--options", "link",
		"--name", inst.VMName,
		"--register"); err != nil {
		return fmt.Errorf("clonar VM: %w", err)
	}

	_ = p.store.AddLog("INFO", fmt.Sprintf("Iniciando VM %s", inst.VMName), inst.ID)
	if err = p.vbm("startvm", inst.VMName, "--type", "headless"); err != nil {
		return fmt.Errorf("iniciar VM: %w", err)
	}

	// Asigna IP fija y espera SSH. La plantilla Debian debe usar networking clásico
	// vía /etc/network/interfaces; netplan o NetworkManager requieren adaptación.
	ip, err := p.assignAndWaitForIP(inst, reservedIP)
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
	if err = p.sshProvision(inst, sqlContent); err != nil {
		return fmt.Errorf("provisionar DB: %w", err)
	}

	inst.Status = models.StatusRunning
	if err = p.store.UpdateInstance(inst); err != nil {
		return fmt.Errorf("actualizar instancia: %w", err)
	}
	_ = p.store.AddLog("OK", fmt.Sprintf("Instancia %s lista — host asignado %s", inst.DBName, inst.Host), inst.ID)
	return nil
}

// assignAndWaitForIP:
// 1. Espera SSH en la IP de plantilla (.10 para MariaDB, .11 para PostgreSQL)
// 2. Asigna una IP libre del rango .20-.254 cambiando solo la línea address
// 3. Espera SSH en la nueva IP
func (p *Provisioner) assignAndWaitForIP(inst *models.Instance, newIP string) (string, error) {

	// IP con la que arranca la plantilla según configuración previa
	var templateIP string
	if inst.Engine == models.EngineMariaDB {
		templateIP = fmt.Sprintf("%s.10", p.cfg.BaseIP)
	} else {
		templateIP = fmt.Sprintf("%s.11", p.cfg.BaseIP)
	}

	_ = p.store.AddLog("INFO", fmt.Sprintf("Esperando SSH inicial en %s", templateIP), inst.ID)
	if err := p.waitForSSH(templateIP, 120*time.Second); err != nil {
		return "", fmt.Errorf("SSH no disponible en IP de plantilla %s: %w", templateIP, err)
	}
	_ = p.store.AddLog("OK", fmt.Sprintf("SSH disponible en %s", templateIP), inst.ID)

	// Cambiar solo la línea address de la interfaz host-only y reiniciar networking sin bloquear la sesión
	changeIPCmd := fmt.Sprintf(
		`sed -i -E 's/^([[:space:]]*address )[0-9.]+$/\1%s/' /etc/network/interfaces && nohup sh -c 'systemctl restart networking >/tmp/nimbus-networking.log 2>&1' >/dev/null 2>&1 < /dev/null &`,
		newIP,
	)
	_ = p.store.AddLog("INFO", fmt.Sprintf("Reconfigurando IP: %s → %s", templateIP, newIP), inst.ID)
	if err := p.runSSH(templateIP, changeIPCmd); err != nil {
		return "", fmt.Errorf("cambiar IP en VM: %w", err)
	}

	// Esperar SSH en la nueva IP
	_ = p.store.AddLog("INFO", fmt.Sprintf("Esperando SSH en nueva IP %s", newIP), inst.ID)
	if err := p.waitForSSH(newIP, 120*time.Second); err != nil {
		return "", fmt.Errorf("SSH no disponible en nueva IP %s: %w", newIP, err)
	}
	_ = p.store.AddLog("OK", fmt.Sprintf("SSH disponible en %s", newIP), inst.ID)

	return newIP, nil
}

func (p *Provisioner) reserveNextIP() (string, error) {
	p.ipMu.Lock()
	defer p.ipMu.Unlock()

	now := time.Now()
	p.cleanupExpiredReservationsLocked(now)

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
		if used[candidate] {
			continue
		}
		if _, reserved := p.reservedIPs[candidate]; reserved {
			continue
		}
		if !isIPAvailable(candidate) {
			continue
		}
		p.reservedIPs[candidate] = now.Add(reservationTTL)
		return candidate, nil
	}
	return "", fmt.Errorf("no hay IPs disponibles en el rango %s.20 – %s.254", p.cfg.BaseIP, p.cfg.BaseIP)
}

func (p *Provisioner) releaseReservedIP(ip string) bool {
	p.ipMu.Lock()
	defer p.ipMu.Unlock()
	if _, ok := p.reservedIPs[ip]; ok {
		delete(p.reservedIPs, ip)
		return true
	}
	return false
}

func (p *Provisioner) cleanupExpiredReservationsLocked(now time.Time) []string {
	expired := make([]string, 0)
	for ip, expiresAt := range p.reservedIPs {
		if now.After(expiresAt) {
			delete(p.reservedIPs, ip)
			expired = append(expired, ip)
		}
	}
	return expired
}

func (p *Provisioner) cleanupExpiredReservations() []string {
	p.ipMu.Lock()
	defer p.ipMu.Unlock()
	return p.cleanupExpiredReservationsLocked(time.Now())
}

func (p *Provisioner) startReservationSweeper(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		expired := p.cleanupExpiredReservations()
		for _, ip := range expired {
			_ = p.store.AddLog("INFO", fmt.Sprintf("Limpieza automática: reserva IP expirada liberada %s", ip), "")
		}
	}
}

func isIPAvailable(ip string) bool {
	var args []string
	if runtime.GOOS == "windows" {
		args = []string{"-n", "1", "-w", "500", ip}
	} else {
		args = []string{"-c", "1", "-W", "1", ip}
	}
	cmd := exec.Command("ping", args...)
	if err := cmd.Run(); err == nil {
		return false
	}
	return true
}

// waitForSSH reintenta hasta que SSH responde o se agota el tiempo
func (p *Provisioner) waitForSSH(ip string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := p.runSSH(ip, "echo ok"); err == nil {
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("timeout esperando SSH en %s", ip)
}

// runSSH ejecuta un comando en la VM vía SSH usando la llave privada
func (p *Provisioner) runSSH(ip, cmd string) error {
	_, err := p.sshRun(ip, cmd)
	return err
}

func (p *Provisioner) runSSHOutput(ip, cmd string) (string, error) {
	return p.sshRun(ip, cmd)
}

func (p *Provisioner) sshRun(ip, cmd string) (string, error) {
	addr := net.JoinHostPort(ip, "22")

	keyBytes, err := os.ReadFile(p.cfg.SSHKeyPath)
	if err != nil {
		return "", fmt.Errorf("leer llave SSH %s: %w", p.cfg.SSHKeyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return "", fmt.Errorf("parsear llave SSH %s: %w", p.cfg.SSHKeyPath, err)
	}

	cfg := &ssh.ClientConfig{
		User:            p.cfg.TemplateUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	conn, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return "", fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	defer conn.Close()

	sess, err := conn.NewSession()
	if err != nil {
		return "", fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()

	var bout, berr bytes.Buffer
	sess.Stdout = &bout
	sess.Stderr = &berr

	if err := sess.Run(cmd); err != nil {
		return "", fmt.Errorf("ssh run %q: %w — stderr: %s", cmd, err, berr.String())
	}
	return bout.String(), nil
}

// sshProvision crea la BD, el usuario y ejecuta el SQL opcional
func (p *Provisioner) sshProvision(inst *models.Instance, sqlContent string) error {
	if err := ValidateIdentifier(inst.DBName, "db_name", inst.Engine); err != nil {
		return err
	}
	if err := ValidateIdentifier(inst.Username, "username", inst.Engine); err != nil {
		return err
	}

	run := func(cmd string) error {
		return p.runSSH(inst.Host, cmd)
	}

	var cmds []string
	if inst.Engine == models.EngineMariaDB {
		cmds = []string{
			fmt.Sprintf(
				"mariadb -u root <<'SQL'\nCREATE DATABASE IF NOT EXISTS `%s`;\nCREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s';\nGRANT ALL PRIVILEGES ON `%s`.* TO '%s'@'%%';\nFLUSH PRIVILEGES;\nSQL",
				inst.DBName, inst.Username, inst.Password, inst.DBName, inst.Username,
			),
		}
	} else {
		cmds = []string{postgresBootstrapCmd(inst.DBName, inst.Username, inst.Password)}
	}

	for _, c := range cmds {
		if err := run(c); err != nil {
			return err
		}
	}

	if sqlContent != "" {
		_ = p.store.AddLog("INFO", fmt.Sprintf("Ejecutando archivo SQL en %s", inst.DBName), inst.ID)
		tmp := fmt.Sprintf("/tmp/nimbus_%s.sql", inst.ID[:8])

		var content string
		if inst.Engine == models.EngineMariaDB {
			content = sanitizeMariaDBSQL(sqlContent)
		} else {
			content = sanitizePostgreSQLSQL(sqlContent)
		}

		if err := run(fmt.Sprintf("cat > %s << 'NEOF'\n%s\nNEOF", tmp, content)); err != nil {
			return err
		}

		var execCmd string
		if inst.Engine == models.EngineMariaDB {
			execCmd = fmt.Sprintf("mariadb -u %s -p%s %s < %s", inst.Username, inst.Password, inst.DBName, tmp)
		} else {
			execCmd = postgresImportCmd(inst.DBName, tmp)
		}
		if err := run(execCmd); err != nil {
			return err
		}
		_ = run("rm " + tmp)
	}
	return nil
}

func sanitizeMariaDBSQL(sqlContent string) string {
	var b strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(sqlContent))
	for scanner.Scan() {
		line := scanner.Text()
		upper := strings.TrimSpace(strings.ToUpper(line))
		if strings.HasPrefix(upper, "CREATE DATABASE ") || strings.HasPrefix(upper, "USE ") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if scanner.Err() != nil {
		return sqlContent
	}
	return b.String()
}

func sanitizePostgreSQLSQL(sqlContent string) string {
	var b strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(sqlContent))
	for scanner.Scan() {
		line := scanner.Text()
		upper := strings.TrimSpace(strings.ToUpper(line))
		if strings.HasPrefix(upper, "CREATE DATABASE ") ||
			strings.HasPrefix(upper, `\C `) ||
			strings.HasPrefix(upper, `\CONNECT `) {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if scanner.Err() != nil {
		return sqlContent
	}
	return b.String()
}

var identifierRegex = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

func ValidateIdentifier(value, field string, engine models.Engine) error {
	if matched := identifierRegex.MatchString(value); !matched {
		return fmt.Errorf("%s solo puede contener letras, números y guion bajo", field)
	}
	reserved := map[string]struct{}{
		"admin":  {},
		"system": {},
	}
	if engine == models.EnginePostgreSQL {
		reserved["postgres"] = struct{}{}
		reserved["template0"] = struct{}{}
		reserved["template1"] = struct{}{}
	} else {
		reserved["mysql"] = struct{}{}
		reserved["root"] = struct{}{}
		reserved["information_schema"] = struct{}{}
		reserved["performance_schema"] = struct{}{}
	}
	if _, found := reserved[strings.ToLower(value)]; found {
		return fmt.Errorf("%s usa un nombre reservado: %s", field, value)
	}
	return nil
}

func escapePostgresLiteral(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}

func postgresBootstrapCmd(dbName, username, password string) string {
	escapedPassword := escapePostgresLiteral(password)
	return fmt.Sprintf(
		`set -e
PSQL=$(command -v psql 2>/dev/null || ls /usr/lib/postgresql/*/bin/psql 2>/dev/null | head -n1)
[ -x "$PSQL" ] || { echo "psql no encontrado" >&2; exit 1; }
DB_EXISTS=$(runuser -u postgres -- "$PSQL" -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname='%s'")
[ -z "$DB_EXISTS" ] && runuser -u postgres -- "$PSQL" -d postgres -c "CREATE DATABASE %s;"
ROLE_EXISTS=$(runuser -u postgres -- "$PSQL" -d postgres -tAc "SELECT 1 FROM pg_roles WHERE rolname='%s'")
if [ -z "$ROLE_EXISTS" ]; then
  runuser -u postgres -- "$PSQL" -d postgres -c "CREATE USER %s WITH PASSWORD '%s';"
else
  runuser -u postgres -- "$PSQL" -d postgres -c "ALTER USER %s WITH PASSWORD '%s';"
fi
runuser -u postgres -- "$PSQL" -d postgres -c "GRANT ALL PRIVILEGES ON DATABASE %s TO %s;"
runuser -u postgres -- "$PSQL" -d %s -c "ALTER SCHEMA public OWNER TO %s;"
runuser -u postgres -- "$PSQL" -d %s -c "GRANT ALL ON SCHEMA public TO %s;"
runuser -u postgres -- "$PSQL" -d %s -c "ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO %s;"`,
		dbName, dbName, username, username, escapedPassword, username, escapedPassword, dbName, username, dbName, username, dbName, username, dbName, username,
	)
}

func postgresImportCmd(dbName, filePath string) string {
	return fmt.Sprintf(
		`set -e
PSQL=$(command -v psql 2>/dev/null || ls /usr/lib/postgresql/*/bin/psql 2>/dev/null | head -n1)
[ -x "$PSQL" ] || { echo "psql no encontrado" >&2; exit 1; }
runuser -u postgres -- "$PSQL" -d %s -v ON_ERROR_STOP=1 -f %s`,
		dbName, filePath,
	)
}

func (p *Provisioner) Destroy(inst *models.Instance) error {
	_ = p.store.AddLog("INFO", fmt.Sprintf("Eliminando instancia %s", inst.DBName), inst.ID)
	if err := p.CleanupVM(inst); err != nil {
		_ = p.store.AddLog("ERROR", fmt.Sprintf("No se pudo eliminar la VM %s: %v", inst.VMName, err), inst.ID)
	}
	_ = p.store.DeleteLogsByInstance(inst.ID)
	_ = p.store.DeleteInstance(inst.ID)
	return p.store.AddLog("OK", fmt.Sprintf("Instancia %s eliminada", inst.DBName), inst.ID)
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
	running, _ := p.isVMRunning(inst.VMName)
	if running {
		_ = p.vbm("controlvm", inst.VMName, "poweroff")
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if r, _ := p.isVMRunning(inst.VMName); !r {
				break
			}
			time.Sleep(1 * time.Second)
		}
	}
	if err := p.vbm("unregistervm", inst.VMName, "--delete"); err != nil {
		return fmt.Errorf("unregistervm %s --delete: %w", inst.VMName, err)
	}
	return nil
}

func (p *Provisioner) rollbackProvision(inst *models.Instance, reservedIP string, cause error) {
	_ = p.store.AddLog("WARN", fmt.Sprintf("Rollback automático de %s: %v", inst.DBName, cause), inst.ID)
	_ = p.store.AddLog("INFO", fmt.Sprintf("Liberando IP reservada %s durante rollback", reservedIP), inst.ID)
	if p.releaseReservedIP(reservedIP) {
		_ = p.store.AddLog("OK", fmt.Sprintf("IP reservada liberada: %s", reservedIP), inst.ID)
	}
	if err := p.CleanupVM(inst); err != nil {
		_ = p.store.AddLog("ERROR", fmt.Sprintf("Rollback fallido al limpiar %s: %v", inst.VMName, err), inst.ID)
		return
	}
	_ = p.store.AddLog("OK", fmt.Sprintf("Rollback exitoso para %s", inst.DBName), inst.ID)
}

func (p *Provisioner) vbm(args ...string) error {
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
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(msg))
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
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(msg))
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
		return "", fmt.Errorf(
			"la plantilla %s no existe en VirtualBox — créala siguiendo las instrucciones del README",
			templateName,
		)
	}
	if err := p.ensureTemplateSnapshot(templateName); err != nil {
		return "", err
	}
	return templateName, nil
}

func (p *Provisioner) ensureTemplateSnapshot(templateName string) error {
	has, err := p.snapshotExists(templateName, p.cfg.TemplateSnapshot)
	if err != nil {
		return err
	}
	if !has {
		return fmt.Errorf(
			"la plantilla %s no tiene el snapshot '%s' — créalo con: VBoxManage snapshot \"%s\" take \"%s\"",
			templateName, p.cfg.TemplateSnapshot, templateName, p.cfg.TemplateSnapshot,
		)
	}
	return nil
}

func (p *Provisioner) snapshotExists(vmName, snapshotName string) (bool, error) {
	out, err := p.vbmOutput("snapshot", vmName, "list", "--machinereadable")
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "does not have any snapshots") ||
			strings.Contains(msg, "vbox_e_object_not_found") {
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
	return strings.Contains(out, fmt.Sprintf(`"%s"`, vmName)), nil
}

func (p *Provisioner) templateForEngine(engine models.Engine) string {
	if engine == models.EnginePostgreSQL {
		return p.cfg.PostgreSQLTemplate
	}
	return p.cfg.MariaDBTemplate
}

// ── ISO helpers (usados solo si se quiere autobootstrap) ──────────────────

func (p *Provisioner) ensureDebianISO() error {
	if ok, _ := isValidISOFile(p.cfg.TemplateISOPath); ok {
		return nil
	}
	_ = os.Remove(p.cfg.TemplateISOPath)
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
	f, err := os.Create(p.cfg.TemplateISOPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func isValidISOFile(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, nil
	}
	if info.Size() < 50*1024*1024 {
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if _, err := f.Seek(32769, io.SeekStart); err != nil {
		return false, nil
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(f, buf); err != nil {
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
		switch inst.Status {
		case models.StatusRunning:
			running, err := p.isVMRunning(inst.VMName)
			if err != nil {
				log.Printf("[health-check] error verificando %s: %v", inst.VMName, err)
				continue
			}
			if !running {
				if p.registerHealthFailure(inst.VMName) < healthCheckFailureThreshold {
					_ = p.store.AddLog("INFO",
						fmt.Sprintf("Health-check: lectura transitoria no confirmó %s; reintentando (%d/%d)", inst.VMName, p.currentHealthFailures(inst.VMName), healthCheckFailureThreshold), inst.ID)
					continue
				}
				inst.Status = models.StatusStopped
				_ = p.store.UpdateInstance(inst)
				_ = p.store.AddLog("WARN",
					fmt.Sprintf("La VM de la instancia %s parece detenida tras %d verificaciones fallidas consecutivas", inst.DBName, healthCheckFailureThreshold), inst.ID)
				p.resetHealthFailure(inst.VMName)
			} else {
				p.resetHealthFailure(inst.VMName)
			}
		case models.StatusStopped:
			running, err := p.isVMRunning(inst.VMName)
			if err == nil && running {
				inst.Status = models.StatusRunning
				_ = p.store.UpdateInstance(inst)
				_ = p.store.AddLog("OK",
					fmt.Sprintf("La VM de la instancia %s se reinició", inst.DBName), inst.ID)
				p.resetHealthFailure(inst.VMName)
			}
		}
	}
}

func (p *Provisioner) registerHealthFailure(vmName string) int {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	p.downCount[vmName]++
	return p.downCount[vmName]
}

func (p *Provisioner) currentHealthFailures(vmName string) int {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	return p.downCount[vmName]
}

func (p *Provisioner) resetHealthFailure(vmName string) {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	delete(p.downCount, vmName)
}

func (p *Provisioner) isVMRunning(vmName string) (bool, error) {
	out, err := p.vbmOutput("showvminfo", vmName, "--machinereadable")
	if err != nil {
		if strings.Contains(err.Error(), "could not find a registered virtual machine") {
			return false, nil
		}
		return false, err
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "VMState=") {
			state := strings.Trim(strings.TrimPrefix(line, "VMState="), `"`)
			if state == "running" {
				return true, nil
			}
			break
		}
	}
	runningOut, err := p.vbmOutput("list", "runningvms")
	if err != nil {
		return false, err
	}
	return strings.Contains(runningOut, fmt.Sprintf(`"%s"`, vmName)), nil
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
