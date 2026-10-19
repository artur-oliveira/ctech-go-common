// Package health serves the two CTech health endpoints in the
// draft-inadarei-api-health-check shape (application/health+json): a
// dependency-free liveness probe and a dependency report built from registered
// checks. See README.md for the fail-open/closed decisions.
package health

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"

	"gopkg.aoctech.app/api-commons/observability"
)

// ContentType is the media type of both endpoints.
const ContentType = "application/health+json"

// Check statuses (draft-inadarei-api-health-check).
const (
	StatusPass = "pass"
	StatusWarn = "warn"
	StatusFail = "fail"
)

// StatusMultiStatus is the HTTP code of a report with a degraded (warn) check:
// the instance still serves traffic and must stay in rotation.
const StatusMultiStatus = 207

// DefaultTimeout is the budget of each check.
const DefaultTimeout = 2 * time.Second

// Unavailable is the observedValue of a check that could not be measured,
// distinct from a measured zero.
const Unavailable = -1

// Component identity of the built-in uptime entry.
const (
	ComponentServer = "server"
	MeasureUptime   = "uptime"
	TypeSystem      = "system"
	UnitSecond      = "second"
)

// Observation is what a probe measured. Warn marks a degraded-but-working
// reading (for example high CPU).
type Observation struct {
	Measurement string
	Value       float64
	Unit        string
	Warn        bool
}

// Check is one registered dependency check.
type Check struct {
	// Name is the key in the report's "checks" map and the component name.
	Name string
	// Type is the componentType, e.g. "datastore" or "system".
	Type string
	// Primary makes a probe error answer fail (HTTP 503). Every other check
	// degrades to warn (HTTP 207) on error, so a secondary dependency can never
	// pull an instance out of rotation.
	Primary bool
	// Probe measures the dependency. A non-nil error means unreachable; its text
	// goes to the log, never to the (unauthenticated) response.
	Probe func(ctx context.Context) (Observation, error)
}

// Config identifies the service in both responses.
type Config struct {
	ServiceID   string
	Description string
	// Version is the API version string, e.g. "/v1".
	Version   string
	ReleaseID string
	// Timeout bounds each check; DefaultTimeout when zero.
	Timeout time.Duration
}

// Handler holds the registered checks. Register checks before serving; it is
// not safe to Add concurrently with requests.
type Handler struct {
	cfg    Config
	start  time.Time
	checks []Check
}

// New returns a Handler with the uptime entry built in.
func New(cfg Config) *Handler {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	return &Handler{cfg: cfg, start: time.Now()}
}

// Add registers a check and returns the Handler for chaining.
func (h *Handler) Add(c ...Check) *Handler {
	h.checks = append(h.checks, c...)
	return h
}

// Entry is one component of the report.
type Entry struct {
	ComponentName   string  `json:"componentName"`
	MeasurementName string  `json:"measurementName"`
	ComponentType   string  `json:"componentType"`
	ObservedValue   float64 `json:"observedValue"`
	ObservedUnit    string  `json:"observedUnit"`
	Status          string  `json:"status"`
	Time            string  `json:"time"`
	// Output is a non-sensitive descriptor, never a dependency's error text.
	Output string `json:"output,omitempty"`
}

// Report is the /health-check body.
type Report struct {
	Status      string           `json:"status"`
	Version     string           `json:"version"`
	ReleaseID   string           `json:"releaseId"`
	ServiceID   string           `json:"serviceId"`
	Description string           `json:"description"`
	Checks      map[string]Entry `json:"checks"`
}

// Liveness is the dependency-free probe: always 200 while the process is up.
// Point the load balancer here, never at Report.
func (h *Handler) Liveness() fiber.Handler {
	body := map[string]string{"status": StatusPass, "releaseId": h.cfg.ReleaseID, "serviceId": h.cfg.ServiceID}
	return func(c fiber.Ctx) error {
		return jsonAs(c, fiber.StatusOK, body)
	}
}

// Report runs every check concurrently, each under its own timeout, and
// answers 503 only if a Primary check failed, 207 if any check warned, else 200.
func (h *Handler) Report() fiber.Handler {
	return func(c fiber.Ctx) error {
		report, code := h.Run(c.Context())
		return jsonAs(c, code, report)
	}
}

// Run executes the checks and returns the report with its HTTP status.
func (h *Handler) Run(ctx context.Context) (Report, int) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	entries := make(map[string]Entry, len(h.checks)+1)
	entries[MeasureUptime] = Entry{
		ComponentName: ComponentServer, MeasurementName: MeasureUptime, ComponentType: TypeSystem,
		ObservedValue: time.Since(h.start).Seconds(), ObservedUnit: UnitSecond, Status: StatusPass, Time: now,
	}

	results := make([]Entry, len(h.checks))
	var wg sync.WaitGroup
	for i, chk := range h.checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = h.runOne(ctx, chk, now)
		}()
	}
	wg.Wait()

	overall, code := StatusPass, fiber.StatusOK
	for i, e := range results {
		entries[h.checks[i].Name] = e
		switch {
		case e.Status == StatusFail:
			overall, code = StatusFail, fiber.StatusServiceUnavailable
		case e.Status == StatusWarn && overall == StatusPass:
			overall, code = StatusWarn, StatusMultiStatus
		}
	}
	return Report{
		Status: overall, Version: h.cfg.Version, ReleaseID: h.cfg.ReleaseID,
		ServiceID: h.cfg.ServiceID, Description: h.cfg.Description, Checks: entries,
	}, code
}

func (h *Handler) runOne(parent context.Context, chk Check, now string) (e Entry) {
	ctx, cancel := context.WithTimeout(parent, h.cfg.Timeout)
	defer cancel()
	e = Entry{ComponentName: chk.Name, ComponentType: chk.Type, Time: now, ObservedValue: Unavailable}

	started := time.Now()
	obs, err := safeProbe(ctx, chk)
	e.MeasurementName, e.ObservedUnit = obs.Measurement, obs.Unit
	if e.MeasurementName == "" { // probe failed before measuring
		e.MeasurementName, e.ObservedUnit = MeasureResponse, UnitMillisecond
	}
	if err != nil {
		e.Status = StatusWarn
		if chk.Primary {
			e.Status = StatusFail
		}
		observability.Warn(ctx, "health check failed", err, "component", chk.Name, "status", e.Status,
			"elapsed", time.Since(started).String())
		return e
	}
	e.ObservedValue, e.Status = obs.Value, StatusPass
	if obs.Warn {
		e.Status = StatusWarn
	}
	return e
}

// safeProbe turns a panic or a nil Probe into an error so one bad check cannot
// take the endpoint down.
func safeProbe(ctx context.Context, chk Check) (obs Observation, err error) {
	if chk.Probe == nil {
		return obs, fmt.Errorf("health: check %q has no probe", chk.Name)
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("health: check %q panicked: %v", chk.Name, r)
		}
	}()
	return chk.Probe(ctx)
}

// jsonAs sends v as application/health+json (c.JSON would reset the type).
func jsonAs(c fiber.Ctx, code int, v any) error {
	c.Status(code)
	if err := c.JSON(v); err != nil {
		return err
	}
	c.Set(fiber.HeaderContentType, ContentType)
	return nil
}
