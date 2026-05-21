package provisioner

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
	"os"
	"os/exec"
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
	BaseIP             string
	VBoxManage         string
	Simulated          bool
}

func DefaultConfig() Config {
	sim := os.Getenv("NIMBUS_SIMULATED")
	simulated := sim == "" || sim == "1" || strings.ToLower(sim) == "true"
	return Config{
		MariaDBTemplate:    getEnv("NIMBUS_MARIADB_TEMPLATE", "nimbus-mariadb-template"),
		PostgreSQLTemplate: getEnv("NIMBUS_PG_TEMPLATE", "nimbus-pg-template"),
		HostOnlyNet:        getEnv("NIMBUS_HOST_ONLY_NET", "vboxnet0"),
		SSHKeyPath:         getEnv("NIMBUS_SSH_KEY", os.Getenv("HOME")+"/.ssh/nimbus_id_rsa"),
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
	return &Provisioner{cfg: cfg, store: s}
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

	_ = p.store.AddLog("INFO", fmt.Sprintf("Clonando plantilla %s → %s", template, inst.VMName), inst.ID)
	if err := p.vbm("clonevm", template, "--name", inst.VMName, "--options", "link", "--snapshot", "base", "--register"); err != nil {
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
		return p.store.AddLog("OK", fmt.Sprintf("Instancia %s eliminada correctamente", inst.DBName), inst.ID)
	}
	_ = p.vbm("controlvm", inst.VMName, "poweroff")
	time.Sleep(2 * time.Second)
	if err := p.vbm("unregistervm", inst.VMName, "--delete"); err != nil {
		return fmt.Errorf("eliminar VM: %w", err)
	}
	_ = p.store.DeleteInstance(inst.ID)
	return p.store.AddLog("OK", fmt.Sprintf("Instancia %s eliminada correctamente", inst.DBName), inst.ID)
}

func (p *Provisioner) vbm(args ...string) error {
	cmd := exec.Command(p.cfg.VBoxManage, args...)
	var buf bytes.Buffer
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, buf.String())
	}
	return nil
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
