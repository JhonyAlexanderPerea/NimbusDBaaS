package models

import "time"

type Engine string

const (
	EngineMariaDB    Engine = "mariadb"
	EnginePostgreSQL Engine = "postgresql"
)

type Status string

const (
	StatusProvisioning Status = "provisioning"
	StatusRunning      Status = "running"
	StatusDeleting     Status = "deleting"
	StatusError        Status = "error"
	StatusDeleted      Status = "deleted"
	StatusStopped      Status = "stopped"
)

type Instance struct {
	ID         string    `json:"id"`
	DBName     string    `json:"db_name"`
	Username   string    `json:"username"`
	Password   string    `json:"password"`
	Engine     Engine    `json:"engine"`
	Status     Status    `json:"status"`
	Host       string    `json:"host"`
	Port       int       `json:"port"`
	VMName     string    `json:"vm_name"`
	CreatedAt  time.Time `json:"created_at"`
	AccessCmd  string    `json:"access_cmd"`
	ErrorMsg   string    `json:"error_msg,omitempty"`
	SQLContent string    `json:"sql_content,omitempty"`
}

type LogEntry struct {
	ID         int64     `json:"id"`
	Level      string    `json:"level"`
	Message    string    `json:"message"`
	InstanceID string    `json:"instance_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type CreateRequest struct {
	DBName     string `json:"db_name"`
	Username   string `json:"username"`
	Engine     Engine `json:"engine"`
	SQLContent string `json:"sql_content,omitempty"`
}
