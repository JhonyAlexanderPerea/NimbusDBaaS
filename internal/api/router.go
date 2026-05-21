package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"nimbusDBaaS/internal/models"
	"nimbusDBaaS/internal/provisioner"
	"nimbusDBaaS/internal/store"
)

type Router struct {
	store *store.Store
	prov  *provisioner.Provisioner
}

func NewRouter(s *store.Store) http.Handler {
	cfg := provisioner.DefaultConfig()
	prov := provisioner.New(cfg, s)

	r := &Router{store: s, prov: prov}
	mux := http.NewServeMux()

	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))
	mux.HandleFunc("/", r.handleIndex)
	mux.HandleFunc("/api/instances", r.handleInstances)
	mux.HandleFunc("/api/instances/", r.handleInstance)
	mux.HandleFunc("/api/logs", r.handleLogs)

	return mux
}

func (r *Router) handleIndex(w http.ResponseWriter, req *http.Request) {
	http.ServeFile(w, req, "web/templates/index.html")
}

func (r *Router) handleInstances(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		instances, err := r.store.ListInstances()
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		if instances == nil {
			instances = []*models.Instance{}
		}
		jsonOK(w, instances)
	case http.MethodPost:
		r.createInstance(w, req)
	default:
		jsonError(w, "method not allowed", 405)
	}
}

func (r *Router) createInstance(w http.ResponseWriter, req *http.Request) {
	var cr models.CreateRequest
	var sqlContent string

	ct := req.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := req.ParseMultipartForm(10 << 20); err != nil {
			jsonError(w, "parse form: "+err.Error(), 400)
			return
		}
		cr.DBName = req.FormValue("db_name")
		cr.Username = req.FormValue("username")
		cr.Engine = models.Engine(req.FormValue("engine"))
		if f, _, err := req.FormFile("sql_file"); err == nil {
			defer f.Close()
			b, _ := io.ReadAll(f)
			sqlContent = string(b)
		}
	} else {
		if err := json.NewDecoder(req.Body).Decode(&cr); err != nil {
			jsonError(w, "invalid JSON: "+err.Error(), 400)
			return
		}
		sqlContent = cr.SQLContent
	}

	if cr.DBName == "" || cr.Username == "" {
		jsonError(w, "db_name y username son requeridos", 400)
		return
	}
	if cr.Engine != models.EngineMariaDB && cr.Engine != models.EnginePostgreSQL {
		jsonError(w, "engine debe ser 'mariadb' o 'postgresql'", 400)
		return
	}

	cr.DBName = sanitize(cr.DBName)
	cr.Username = sanitize(cr.Username)

	inst := &models.Instance{
		ID:         newUUID(),
		DBName:     cr.DBName,
		Username:   cr.Username,
		Password:   genPassword(),
		Engine:     cr.Engine,
		Status:     models.StatusProvisioning,
		CreatedAt:  time.Now().UTC(),
		SQLContent: sqlContent,
	}

	if err := r.store.CreateInstance(inst); err != nil {
		jsonError(w, "store: "+err.Error(), 500)
		return
	}

	r.prov.Provision(inst, sqlContent)

	w.WriteHeader(http.StatusAccepted)
	jsonOK(w, inst)
}

func (r *Router) handleInstance(w http.ResponseWriter, req *http.Request) {
	suffix := strings.TrimPrefix(req.URL.Path, "/api/instances/")
	if suffix == "" {
		jsonError(w, "missing id", 400)
		return
	}
	if strings.HasSuffix(suffix, "/logs") {
		r.clearInstanceLogs(w, req, strings.TrimSuffix(suffix, "/logs"))
		return
	}
	if strings.HasSuffix(suffix, "/retry") {
		r.retryInstance(w, req, strings.TrimSuffix(suffix, "/retry"))
		return
	}
	id := suffix
	switch req.Method {
	case http.MethodGet:
		inst, err := r.store.GetInstance(id)
		if err != nil {
			jsonError(w, fmt.Sprintf("instance %s not found", id), 404)
			return
		}
		jsonOK(w, inst)
	case http.MethodDelete:
		inst, err := r.store.GetInstance(id)
		if err != nil {
			jsonError(w, "not found", 404)
			return
		}
		go func() {
			if err := r.prov.Destroy(inst); err != nil {
				_ = r.store.AddLog("ERROR", fmt.Sprintf("Error eliminando %s: %v", inst.DBName, err), inst.ID)
			}
		}()
		jsonOK(w, map[string]string{"status": "deleting"})
	default:
		jsonError(w, "method not allowed", 405)
	}
}

func (r *Router) retryInstance(w http.ResponseWriter, req *http.Request, id string) {
	if req.Method != http.MethodPost {
		jsonError(w, "method not allowed", 405)
		return
	}
	inst, err := r.store.GetInstance(id)
	if err != nil {
		jsonError(w, "not found", 404)
		return
	}
	if inst.Status != models.StatusError {
		jsonError(w, "solo se puede reintentar una instancia en estado error", 409)
		return
	}
	if err := r.prov.CleanupVM(inst); err != nil {
		_ = r.store.AddLog("WARN", fmt.Sprintf("No se pudo limpiar la VM %s antes de reintentar: %v", inst.VMName, err), inst.ID)
	}
	inst.Status = models.StatusProvisioning
	inst.ErrorMsg = ""
	inst.Host = ""
	inst.Port = 0
	inst.AccessCmd = ""
	if err := r.store.UpdateInstance(inst); err != nil {
		jsonError(w, "store: "+err.Error(), 500)
		return
	}
	_ = r.store.AddLog("INFO", fmt.Sprintf("Reintentando provisión de %s", inst.DBName), inst.ID)
	r.prov.Provision(inst, inst.SQLContent)
	jsonOK(w, map[string]string{"status": "retrying"})
}

func (r *Router) clearInstanceLogs(w http.ResponseWriter, req *http.Request, id string) {
	if req.Method != http.MethodDelete {
		jsonError(w, "method not allowed", 405)
		return
	}
	if _, err := r.store.GetInstance(id); err != nil {
		jsonError(w, "not found", 404)
		return
	}
	if err := r.store.DeleteLogsByInstance(id); err != nil {
		jsonError(w, "store: "+err.Error(), 500)
		return
	}
	_ = r.store.AddLog("OK", fmt.Sprintf("Logs limpiados para la instancia %s", id), id)
	jsonOK(w, map[string]string{"status": "logs-cleared"})
}

func (r *Router) handleLogs(w http.ResponseWriter, req *http.Request) {
	logs, err := r.store.ListLogs(200)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if logs == nil {
		logs = []*models.LogEntry{}
	}
	jsonOK(w, logs)
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func genPassword() string {
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:12]
}
