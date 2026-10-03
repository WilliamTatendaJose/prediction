package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/williamtatendajose/prediction/iot-hub/internal/auth"
	"github.com/williamtatendajose/prediction/iot-hub/internal/twin"
)

// Commands: a catalog of named commands (direct methods or messages) that
// admins define and operators run. See twin/commands.go.

// mayRun: admins (and services, on devices they control) run any command;
// operators run those marked for operators.
func mayRun(id *auth.Identity, c twin.Command, dev string) bool {
	return id.Can(auth.Control, dev) || c.Role != "admin" && id.Can(auth.Operate, "")
}

// commandDevices are the devices and services the caller may run commands on.
func (s *Server) commandDevices(id *auth.Identity) []string {
	var out []string
	for _, d := range s.Auth.List() {
		if (d.Role == auth.Device || d.Role == auth.Service) && (id.Can(auth.Control, d.ID) || id.Can(auth.Operate, "")) {
			out = append(out, d.ID)
		}
	}
	return out
}

func (s *Server) runnable(id *auth.Identity, dev string) []twin.Command {
	out := []twin.Command{}
	for _, c := range s.Twins.For(dev) {
		if mayRun(id, c, dev) {
			out = append(out, c)
		}
	}
	return out
}

type commandDevice struct {
	ID              string   `json:"id"`
	ConnectionState string   `json:"connectionState"`
	LastData        int64    `json:"lastDataTime,omitempty"`
	Expected        int64    `json:"expectedIntervalSec,omitempty"`
	Commands        []string `json:"commands"`
}

// listCommands: GET /api/commands → the catalog, the devices the caller may
// command (with what each offers), and recent runs.
func (s *Server) listCommands(w http.ResponseWriter, r *http.Request) {
	if !s.twinsOn(w) {
		return
	}
	id := caller(r)
	if !id.Can(auth.Operate, "") && !id.Can(auth.Manage, "") {
		writeErr(w, http.StatusForbidden, errors.New("commands are for operators and admins"))
		return
	}
	ids := s.commandDevices(id)
	devs := []commandDevice{}
	for _, v := range s.Twins.Query(ids, nil) {
		d := commandDevice{ID: v.DeviceID, ConnectionState: v.ConnectionState, LastData: v.LastData, Expected: v.Expected, Commands: []string{}}
		for _, c := range s.runnable(id, v.DeviceID) {
			d.Commands = append(d.Commands, c.Name)
		}
		devs = append(devs, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"commands": s.Twins.Commands(), "devices": devs, "history": s.Twins.RecentRuns(ids, 100), "batches": s.Twins.Batches(),
		"canEdit": id.Can(auth.Manage, ""),
	})
}

// putCommand: PUT /api/commands/{name} defines or replaces a command.
func (s *Server) putCommand(w http.ResponseWriter, r *http.Request) {
	if !s.twinsOn(w) {
		return
	}
	var c twin.Command
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*twin.MaxMessageBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if c.Name == "" {
		c.Name = r.PathValue("name")
	}
	if c.Name != r.PathValue("name") {
		writeErr(w, http.StatusBadRequest, errors.New("name in the body differs from the path"))
		return
	}
	c, err := s.Twins.SetCommand(c)
	if err != nil {
		storeErr(w, err)
		return
	}
	s.audit(r, "command.define", c.Name, c.Kind+" "+c.MethodName())
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) deleteCommand(w http.ResponseWriter, r *http.Request) {
	if !s.twinsOn(w) {
		return
	}
	if err := s.Twins.DeleteCommand(r.PathValue("name")); err != nil {
		storeErr(w, err)
		return
	}
	s.audit(r, "command.delete", r.PathValue("name"), "")
	w.WriteHeader(http.StatusNoContent)
}

// deviceCommands: GET /api/devices/{id}/commands → what the caller may run
// there, and the device's history.
func (s *Server) deviceCommands(w http.ResponseWriter, r *http.Request) {
	if !s.twinsOn(w) {
		return
	}
	id, dev := caller(r), r.PathValue("id")
	if !s.mayCommand(w, id, dev) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"commands": s.runnable(id, dev), "history": s.Twins.Runs(dev)})
}

func (s *Server) mayCommand(w http.ResponseWriter, id *auth.Identity, dev string) bool {
	for _, d := range s.commandDevices(id) {
		if d == dev {
			return true
		}
	}
	if id.Can(auth.Operate, "") || id.Can(auth.Control, dev) {
		writeErr(w, http.StatusNotFound, errors.New("no device or service "+dev))
	} else {
		writeErr(w, http.StatusForbidden, errors.New(id.ID+" ("+string(id.Role)+") may not send commands"))
	}
	return false
}

// runCommand: POST /api/devices/{id}/commands/{name} {"params": {...}}
func (s *Server) runCommand(w http.ResponseWriter, r *http.Request) {
	if !s.twinsOn(w) {
		return
	}
	id, dev, name := caller(r), r.PathValue("id"), r.PathValue("name")
	if !s.mayCommand(w, id, dev) {
		return
	}
	var c *twin.Command
	for _, x := range s.Twins.For(dev) {
		if x.Name == name {
			c = &x
		}
	}
	if c == nil {
		writeErr(w, http.StatusNotFound, errors.New("command "+name+" is not offered on "+dev))
		return
	}
	if !mayRun(id, *c, dev) {
		writeErr(w, http.StatusForbidden, errors.New("only admins may run "+name))
		return
	}
	var req struct {
		Params map[string]any `json:"params"`
	}
	if r.ContentLength != 0 {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, twin.MaxMessageBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	run, err := s.Twins.RunCommand(r.Context(), dev, name, id.ID, req.Params)
	if err != nil {
		if errors.Is(err, twin.ErrNotOffered) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		twinErr(w, err)
		return
	}
	s.audit(r, "command.run", dev, name+" → "+run.Status)
	writeJSON(w, http.StatusOK, run)
}

// setExpected: PUT /api/devices/{id}/expected-interval {"interval": "5m"} ("" clears)
func (s *Server) setExpected(w http.ResponseWriter, r *http.Request) {
	if !s.twinsOn(w) {
		return
	}
	var req struct {
		Interval string `json:"interval"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var d time.Duration
	if req.Interval != "" {
		var err error
		if d, err = time.ParseDuration(req.Interval); err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("interval: a duration such as 30s, 5m or 1h"))
			return
		}
	}
	dev := r.PathValue("id")
	if err := s.Twins.SetExpected(dev, d); err != nil {
		storeErr(w, err)
		return
	}
	s.audit(r, "device.expected", dev, req.Interval)
	w.WriteHeader(http.StatusNoContent)
}

// runBatch: POST /api/commands/{name}/run {"devices": [...], "params": {...}}
// sends one command to many devices in the background → 202 with the batch;
// poll GET /api/commands/batches/{id}. Devices the caller may not command
// this way are reported as skipped.
func (s *Server) runBatch(w http.ResponseWriter, r *http.Request) {
	if !s.twinsOn(w) {
		return
	}
	id, name := caller(r), r.PathValue("name")
	if !id.Can(auth.Operate, "") && !id.Can(auth.Manage, "") && id.Role != auth.Service {
		writeErr(w, http.StatusForbidden, errors.New("commands are for operators and admins"))
		return
	}
	var req struct {
		Devices []string          `json:"devices"`
		Params  map[string]any    `json:"params"`
		Retry   *twin.RetryPolicy `json:"retry"` // omitted: the command's default; attempts 0: none
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, twin.MaxMessageBody+64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Devices) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("choose at least one device"))
		return
	}
	var c *twin.Command
	for _, x := range s.Twins.Commands() {
		if x.Name == name {
			c = &x
		}
	}
	if c == nil {
		writeErr(w, http.StatusNotFound, errors.New("no command "+name))
		return
	}
	run, skip := s.sortDevices(id, *c, req.Devices)
	b, err := s.Twins.RunBatch(name, id.ID, run, skip, req.Params, req.Retry)
	if err != nil {
		storeErr(w, err)
		return
	}
	s.audit(r, "command.batch", name, fmt.Sprintf("%d devices, %d skipped, batch %s", b.Total-b.Counts["skipped"], b.Counts["skipped"], b.ID))
	writeJSON(w, http.StatusAccepted, b)
}

// getBatch: GET /api/commands/batches/{batch}
func (s *Server) getBatch(w http.ResponseWriter, r *http.Request) {
	if !s.twinsOn(w) {
		return
	}
	id := caller(r)
	b, err := s.Twins.GetBatch(r.PathValue("batch"))
	if err != nil {
		storeErr(w, err)
		return
	}
	// People who may command see every batch; an app only its own.
	if !id.Can(auth.Operate, "") && !id.Can(auth.Manage, "") && !(id.Role == auth.Service && b.By == id.ID) {
		writeErr(w, http.StatusForbidden, errors.New("commands are for operators and admins"))
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// sortDevices splits devices into those the caller may run c on and the
// rest, with the reason.
func (s *Server) sortDevices(id *auth.Identity, c twin.Command, devices []string) ([]string, map[string]string) {
	allowed := map[string]bool{}
	for _, d := range s.commandDevices(id) {
		allowed[d] = true
	}
	var run []string
	skip := map[string]string{}
	for _, d := range devices {
		switch {
		case !allowed[d]:
			skip[d] = "not a device you may command"
		case !mayRun(id, c, d):
			skip[d] = "only admins may run this command"
		default:
			run = append(run, d)
		}
	}
	return run, skip
}

// resendBatch: POST /api/commands/batches/{batch}/resend sends the same
// command and parameters again to the devices that failed (offline, no
// answer, refused, interrupted, undelivered message) → 202, a new batch.
func (s *Server) resendBatch(w http.ResponseWriter, r *http.Request) {
	if !s.twinsOn(w) {
		return
	}
	id := caller(r)
	if !id.Can(auth.Operate, "") && !id.Can(auth.Manage, "") && id.Role != auth.Service {
		writeErr(w, http.StatusForbidden, errors.New("commands are for operators and admins"))
		return
	}
	of, failed, err := s.Twins.Failed(r.PathValue("batch"))
	if err != nil {
		storeErr(w, err)
		return
	}
	if id.Role == auth.Service && of.By != id.ID {
		writeErr(w, http.StatusForbidden, errors.New("an app may only resend its own batches"))
		return
	}
	if of.Finished == 0 {
		writeErr(w, http.StatusConflict, errors.New("the batch is still running"))
		return
	}
	if len(failed) == 0 {
		writeErr(w, http.StatusConflict, errors.New("no device in this batch failed"))
		return
	}
	var c *twin.Command
	for _, x := range s.Twins.Commands() {
		if x.Name == of.Command {
			c = &x
		}
	}
	if c == nil {
		writeErr(w, http.StatusNotFound, errors.New("command "+of.Command+" no longer exists"))
		return
	}
	run, skip := s.sortDevices(id, *c, failed)
	b, err := s.Twins.Resend(of, id.ID, run, skip)
	if err != nil {
		storeErr(w, err)
		return
	}
	s.audit(r, "command.resend", of.Command, fmt.Sprintf("batch %s → %s, %d devices", of.ID, b.ID, len(run)))
	writeJSON(w, http.StatusAccepted, b)
}

// cancelBatch: POST /api/commands/batches/{batch}/cancel stops a batch's
// pending automatic retries.
func (s *Server) cancelBatch(w http.ResponseWriter, r *http.Request) {
	if !s.twinsOn(w) {
		return
	}
	id := caller(r)
	b, err := s.Twins.GetBatch(r.PathValue("batch"))
	if err != nil {
		storeErr(w, err)
		return
	}
	if !id.Can(auth.Operate, "") && !id.Can(auth.Manage, "") && !(id.Role == auth.Service && b.By == id.ID) {
		writeErr(w, http.StatusForbidden, errors.New("commands are for operators and admins"))
		return
	}
	if b, err = s.Twins.CancelBatch(b.ID); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	s.audit(r, "command.cancel", b.Command, "batch "+b.ID)
	writeJSON(w, http.StatusOK, b)
}
