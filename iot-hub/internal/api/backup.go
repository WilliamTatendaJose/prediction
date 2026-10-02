package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/backup"
	"github.com/williamtatendajose/prediction/iot-hub/internal/store"
)

const maxBackupBody = 16 << 20 // 500 sensors x 16 fields with rules is far below this

// exportConfig: GET /api/config?devices=1
func (s *Server) exportConfig(w http.ResponseWriter, r *http.Request) {
	devices := r.URL.Query().Get("devices") == "1"
	c := backup.Export(s.Store, s.Auth, devices)
	name := "iothub-config-" + time.Now().UTC().Format("20060102-150405") + ".json"
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	s.audit(r, "config.export", "config", fmt.Sprintf("%d sensors, devices=%v", len(c.Sensors), devices))
	writeJSON(w, http.StatusOK, c)
}

// importConfig: POST /api/config?mode=merge|replace&dryRun=1
func (s *Server) importConfig(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mode := q.Get("mode")
	if mode == "" {
		mode = "merge"
	}
	if mode != "merge" && mode != "replace" {
		storeErr(w, fmt.Errorf("%w: mode must be merge or replace", store.ErrInvalid))
		return
	}
	var c backup.Config
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBackupBody))
	dec.DisallowUnknownFields() // a typo in a hand-edited backup should fail, not be dropped
	if err := dec.Decode(&c); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	dry := q.Get("dryRun") == "1"
	plan, err := backup.Import(c, s.Store, s.Auth, mode == "replace", dry)
	if err != nil {
		storeErr(w, err)
		return
	}
	if !dry {
		for _, ids := range [][]string{plan.Updated, plan.Deleted} {
			for _, id := range ids {
				if s.Pipeline != nil && s.Pipeline.Calc != nil {
					s.Pipeline.Calc.Forget(id)
				}
				if s.Detector != nil {
					s.Detector.Forget(id)
				}
			}
		}
		if s.OnRevoke != nil {
			for _, id := range plan.Revoked {
				s.OnRevoke(id)
			}
		}
		s.audit(r, "config.import", "config", fmt.Sprintf("mode=%s created=%d updated=%d deleted=%d dashboard=%v devices=%d revoked=%s",
			mode, len(plan.Created), len(plan.Updated), len(plan.Deleted), plan.Dashboard, plan.Devices, strings.Join(plan.Revoked, ",")))
	}
	writeJSON(w, http.StatusOK, plan)
}

var errNoBackups = errors.New("backups are off (set -backup-dir)")

// backups: GET /api/backups
func (s *Server) backups(w http.ResponseWriter, _ *http.Request) {
	if s.Backups == nil {
		writeErr(w, http.StatusConflict, errNoBackups)
		return
	}
	st, err := s.Backups.Status()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// backupNow: POST /api/backups
func (s *Server) backupNow(w http.ResponseWriter, r *http.Request) {
	if s.Backups == nil {
		writeErr(w, http.StatusConflict, errNoBackups)
		return
	}
	files, err := s.Backups.Once(r.Context())
	s.audit(r, "backup.run", "backups", strings.Join(files, ","))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

// backupFile: GET /api/backups/{name}
func (s *Server) backupFile(w http.ResponseWriter, r *http.Request) {
	if s.Backups == nil {
		writeErr(w, http.StatusConflict, errNoBackups)
		return
	}
	name := r.PathValue("name")
	p, ok := s.Backups.Path(name)
	if !ok {
		writeErr(w, http.StatusNotFound, store.ErrNotFound)
		return
	}
	s.audit(r, "backup.download", name, "")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, p)
}
